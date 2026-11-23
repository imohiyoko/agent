// Package mcpgw is the per-namespace MCP gateway (ADR 0004).
//
// Agents talk MCP Streamable HTTP to /mcp/<server>. The gateway starts the
// declared stdio server (one process per MCP session), passes through only
// the methods and tools the namespace allows, and filters tools/list.
package mcpgw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/spec"
)

// Starter returns a ready-to-start command for the server (already wrapped in
// the namespace's sandbox, with its secrets in the environment).
type Starter func(s *spec.MCPServer) (*exec.Cmd, error)

// Gateway serves one namespace.
type Gateway struct {
	ns    atomic.Pointer[spec.Namespace]
	start Starter
	// remote sends requests through the namespace's own proxy, for servers
	// reached by a credential route.
	remote    *http.Client
	remoteKey string
	log       *audit.Log
	idle      time.Duration
	timeout   time.Duration

	mu       sync.Mutex
	sessions map[string]*session
}

// New returns a gateway for ns.
func New(ns *spec.Namespace, start Starter, log *audit.Log) *Gateway {
	g := &Gateway{start: start, log: log, idle: 15 * time.Minute, timeout: 5 * time.Minute, sessions: map[string]*session{}}
	g.ns.Store(ns)
	return g
}

// SetRemote sets the client used for remote servers. It must send requests
// through this namespace's proxy (never directly).
// key is sent as egress.GatewayHeader so the proxy lets the gateway (and
// only the gateway) use gateway-only credential routes.
func (g *Gateway) SetRemote(c *http.Client, key string) { g.remote, g.remoteKey = c, key }

// SetNamespace replaces the rules. Running sessions pick them up on their
// next message.
func (g *Gateway) SetNamespace(ns *spec.Namespace) { g.ns.Store(ns) }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (m *message) isRequest() bool { return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null" }

type session struct {
	id     string
	server string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	mu     sync.Mutex
	wait   map[string]chan message
	last   atomic.Int64
	closed chan struct{}
	once   sync.Once
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.closed)
		_ = s.stdin.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}

func (s *session) send(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (g *Gateway) record(server, action, target string, allowed bool, reason string) {
	g.log.Write(audit.Event{
		Namespace: g.ns.Load().Name, Kind: "mcp", Action: server + ":" + action,
		Target: target, Allowed: allowed, Reason: reason,
	})
}

// ServeHTTP implements MCP Streamable HTTP for /mcp/<server>.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	ns := g.ns.Load()
	srv := ns.MCPServer(name)
	if !ok || srv == nil {
		g.record(name, "connect", "", false, "no such server in this namespace")
		http.Error(w, "nsctl: no MCP server "+name+" in this namespace", http.StatusNotFound)
		return
	}
	if srv.Credential != "" {
		g.serveRemote(w, r, srv)
		return
	}
	switch r.Method {
	case http.MethodPost:
		g.post(w, r, srv)
	case http.MethodDelete:
		if s := g.session(r, name); s != nil {
			g.drop(s)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		// No server-to-client stream in the first stage (ADR 0004).
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (g *Gateway) session(r *http.Request, server string) *session {
	id := r.Header.Get("Mcp-Session-Id")
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	if s == nil || s.server != server {
		return nil
	}
	return s
}

func (g *Gateway) drop(s *session) {
	g.mu.Lock()
	delete(g.sessions, s.id)
	g.mu.Unlock()
	s.close()
}

func rpcError(id json.RawMessage, code int, msg string) message {
	e, _ := json.Marshal(map[string]any{"code": code, "message": msg})
	return message{JSONRPC: "2.0", ID: id, Error: e}
}

func writeJSON(w http.ResponseWriter, m message) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// decision returns "" if the method may pass, or the reason it may not.
func decision(srv *spec.MCPServer, m *message) string {
	switch {
	case m.Method == "initialize", m.Method == "ping", strings.HasPrefix(m.Method, "notifications/"):
		return ""
	case m.Method == "tools/list":
		return ""
	case m.Method == "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		for _, t := range srv.Tools {
			if t == p.Name {
				return ""
			}
		}
		return "tool " + p.Name + " is not allowed in this namespace"
	case strings.HasPrefix(m.Method, "resources/"):
		if srv.AllowResources {
			return ""
		}
		return "resources are not allowed in this namespace"
	case strings.HasPrefix(m.Method, "prompts/"):
		if srv.AllowPrompts {
			return ""
		}
		return "prompts are not allowed in this namespace"
	}
	return "method " + m.Method + " is not allowed"
}

func (g *Gateway) post(w http.ResponseWriter, r *http.Request, srv *spec.MCPServer) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		writeJSON(w, rpcError(nil, -32600, "batches are not supported"))
		return
	}
	var m message
	if err := decodeStrict(body, &m); err != nil {
		writeJSON(w, rpcError(nil, -32700, "parse error"))
		return
	}

	var s *session
	if m.Method == "initialize" && r.Header.Get("Mcp-Session-Id") == "" {
		s, err = g.open(srv)
		if err != nil {
			g.record(srv.Name, "initialize", "", false, "cannot start server")
			http.Error(w, "nsctl: cannot start MCP server "+srv.Name, http.StatusBadGateway)
			return
		}
	} else if s = g.session(r, srv.Name); s == nil {
		http.Error(w, "unknown or expired Mcp-Session-Id", http.StatusNotFound)
		return
	}
	s.last.Store(time.Now().UnixNano())
	w.Header().Set("Mcp-Session-Id", s.id)

	if !m.isRequest() {
		// A notification, or the client's answer to a server request.
		if m.Method != "" && decision(srv, &m) != "" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = s.send(m)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Re-read the rules for every request so a reload applies at once.
	srv = g.ns.Load().MCPServer(srv.Name)
	if srv == nil {
		g.drop(s)
		http.Error(w, "server removed", http.StatusNotFound)
		return
	}
	if reason := decision(srv, &m); reason != "" {
		g.record(srv.Name, m.Method, toolName(&m), false, reason)
		writeJSON(w, rpcError(m.ID, -32601, "nsctl: "+reason))
		return
	}

	ch := make(chan message, 1)
	key := string(m.ID)
	s.mu.Lock()
	if _, dup := s.wait[key]; dup {
		s.mu.Unlock()
		writeJSON(w, rpcError(m.ID, -32600, "duplicate request id"))
		return
	}
	s.wait[key] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.wait, key); s.mu.Unlock() }()

	if err := s.send(m); err != nil {
		g.drop(s)
		http.Error(w, "server is gone", http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()
	select {
	case resp := <-ch:
		if m.Method == "tools/list" {
			resp.Result = filterTools(resp.Result, srv.Tools)
		}
		g.record(srv.Name, m.Method, toolName(&m), true, "")
		writeJSON(w, resp)
	case <-s.closed:
		http.Error(w, "server exited", http.StatusBadGateway)
	case <-ctx.Done():
		writeJSON(w, rpcError(m.ID, -32603, "nsctl: timed out"))
	}
}

func toolName(m *message) string {
	if m.Method != "tools/call" {
		return ""
	}
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return p.Name
}

// filterTools removes tools that are not allowed from a tools/list result.
func filterTools(result json.RawMessage, allowed []string) json.RawMessage {
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil {
		return json.RawMessage(`{"tools":[]}`)
	}
	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(r["tools"], &tools)
	ok := map[string]bool{}
	for _, t := range allowed {
		ok[t] = true
	}
	kept := []map[string]json.RawMessage{}
	for _, t := range tools {
		var name string
		_ = json.Unmarshal(t["name"], &name)
		if ok[name] {
			kept = append(kept, t)
		}
	}
	r["tools"], _ = json.Marshal(kept)
	out, _ := json.Marshal(r)
	return out
}

func (g *Gateway) open(srv *spec.MCPServer) (*session, error) {
	cmd, err := g.start(srv)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	s := &session{
		id: hex.EncodeToString(idb), server: srv.Name, cmd: cmd, stdin: stdin,
		wait: map[string]chan message{}, closed: make(chan struct{}),
	}
	s.last.Store(time.Now().UnixNano())
	go g.read(s, stdout)
	go func() { _ = cmd.Wait(); g.drop(s) }()
	g.mu.Lock()
	g.sessions[s.id] = s
	g.mu.Unlock()
	return s, nil
}

func (g *Gateway) read(s *session, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method != "" {
			if m.isRequest() {
				// Server-to-client requests (sampling, roots...) are not
				// relayed in the first stage. Answer so the server is not stuck.
				g.record(s.server, m.Method, "", false, "server requests are not relayed")
				_ = s.send(rpcError(m.ID, -32601, "not supported by nsctl gateway"))
			}
			continue
		}
		s.mu.Lock()
		ch := s.wait[string(m.ID)]
		s.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// Reap closes sessions idle for longer than the idle timeout. Run it in the
// background.
func (g *Gateway) Reap(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			for _, s := range g.sessions {
				s.close()
			}
			g.mu.Unlock()
			return
		case <-t.C:
			cut := time.Now().Add(-g.idle).UnixNano()
			g.mu.Lock()
			var old []*session
			for _, s := range g.sessions {
				if s.last.Load() < cut {
					old = append(old, s)
				}
			}
			g.mu.Unlock()
			for _, s := range old {
				g.drop(s)
			}
		}
	}
}

// Serve runs the gateway on l until ctx is done.
func (g *Gateway) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go g.Reap(ctx)
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ConfigJSON returns a Claude Code style MCP configuration pointing at the
// gateway address seen inside the sandbox.
func ConfigJSON(ns *spec.Namespace, addr string) ([]byte, error) {
	servers := map[string]any{}
	for _, s := range ns.MCP.Servers {
		servers[s.Name] = map[string]string{"type": "http", "url": fmt.Sprintf("http://%s/mcp/%s", addr, s.Name)}
	}
	return json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
}
