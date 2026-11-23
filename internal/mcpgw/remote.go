package mcpgw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/imohiyoko/agent/internal/spec"
)

// GatewayHeader must match egress.GatewayHeader (mcpgw does not import
// egress; the server test checks they agree).
const GatewayHeader = "X-Nsctl-Gateway"

// serveRemote relays to a remote Streamable HTTP server through the
// namespace's credential route http://<route>.creds.internal<path>. The same
// method and tool checks as for stdio servers apply before anything is sent,
// and tools/list answers are filtered on the way back (JSON or SSE).
func (g *Gateway) serveRemote(w http.ResponseWriter, r *http.Request, srv *spec.MCPServer) {
	if g.remote == nil {
		http.Error(w, "nsctl: remote MCP servers are not configured", http.StatusBadGateway)
		return
	}
	target := "http://" + srv.Credential + spec.CredsSuffix + srv.Path
	var body []byte
	var m message
	switch r.Method {
	case http.MethodPost:
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		body = bytes.TrimSpace(body)
		if len(body) > 0 && body[0] == '[' {
			writeJSON(w, rpcError(nil, -32600, "batches are not supported"))
			return
		}
		if err := json.Unmarshal(body, &m); err != nil {
			writeJSON(w, rpcError(nil, -32700, "parse error"))
			return
		}
		if m.Method != "" {
			if reason := decision(srv, &m); reason != "" {
				g.record(srv.Name, m.Method, toolName(&m), false, reason)
				if m.isRequest() {
					writeJSON(w, rpcError(m.ID, -32601, "nsctl: "+reason))
				} else {
					w.WriteHeader(http.StatusAccepted)
				}
				return
			}
		}
	case http.MethodDelete:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set(GatewayHeader, g.remoteKey)
	resp, err := g.remote.Do(req)
	if err != nil {
		g.record(srv.Name, m.Method, toolName(&m), false, "upstream error")
		http.Error(w, "nsctl: remote MCP server unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if m.isRequest() {
		g.record(srv.Name, m.Method, toolName(&m), resp.StatusCode < 400, resp.Status)
	}
	for _, h := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	isList := m.Method == "tools/list"
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		w.WriteHeader(resp.StatusCode)
		g.relaySSE(w, resp.Body, srv, m.ID, isList)
	case strings.HasPrefix(ct, "application/json") && resp.StatusCode < 300:
		b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		b = g.filterMessage(b, srv, m.ID, isList)
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
	}
}

// filterMessage applies the gateway's rules to one message from the server:
// tools/list results are filtered, server-to-client requests are dropped.
// It returns nil when the message must not reach the agent.
func (g *Gateway) filterMessage(b []byte, srv *spec.MCPServer, reqID json.RawMessage, isList bool) []byte {
	var m message
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if m.isRequest() {
		g.record(srv.Name, m.Method, "", false, "server requests are not relayed")
		return nil
	}
	if isList && m.Method == "" && string(m.ID) == string(reqID) && len(m.Result) > 0 {
		m.Result = filterTools(m.Result, srv.Tools)
		out, err := json.Marshal(m)
		if err != nil {
			return nil
		}
		return out
	}
	return b
}

// relaySSE copies an event stream, passing each event's data through
// filterMessage.
func (g *Gateway) relaySSE(w http.ResponseWriter, body io.Reader, srv *spec.MCPServer, reqID json.RawMessage, isList bool) {
	fl, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var event []string
	var data []string
	flush := func() {
		if len(data) > 0 {
			out := g.filterMessage([]byte(strings.Join(data, "\n")), srv, reqID, isList)
			if out == nil {
				event, data = nil, nil
				return
			}
			for _, l := range event {
				_, _ = io.WriteString(w, l+"\n")
			}
			_, _ = io.WriteString(w, "data: "+string(out)+"\n\n")
		} else if len(event) > 0 {
			for _, l := range event {
				_, _ = io.WriteString(w, l+"\n")
			}
			_, _ = io.WriteString(w, "\n")
		}
		if fl != nil {
			fl.Flush()
		}
		event, data = nil, nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			event = append(event, line) // id:, event:, retry:, comments
		}
	}
	flush()
}
