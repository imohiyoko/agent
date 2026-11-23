package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/auth"
	"github.com/imohiyoko/agent/internal/egress"
	"github.com/imohiyoko/agent/internal/mcpgw"
	"github.com/imohiyoko/agent/internal/sandbox"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

func (rt *nsRuntime) name() string { return rt.proxy.Name() }

// nsRuntime is everything running for one namespace.
type nsRuntime struct {
	ns      *spec.Namespace // replaced on reload; read with Server.mu held
	cancel  context.CancelFunc
	proxy   *egress.Proxy
	gateway *mcpgw.Gateway
	// Linux: unix sockets. macOS: loopback ports.
	proxySock, mcpSock string
	proxyPort, mcpPort int
}

// Server is a running nsctl.
type Server struct {
	cfg     *Config
	setp    atomic.Pointer[spec.Set]
	tokens  *auth.Store
	authz   *auth.Authorizer
	secrets secrets.Source
	log     *audit.Log
	self    string
	goos    string

	// proxyHook adjusts each proxy before it serves (tests only).
	proxyHook func(*egress.Proxy)

	mu   sync.Mutex
	nss  map[string]*nsRuntime
	runs *runs

	// Set while Run is serving, for Reload.
	runCtx context.Context
	errc   chan error
}

// checkOverlap refuses writable paths that contain, or are inside, the nsctl
// state directory or the secrets file.
func checkOverlap(cfg *Config, set *spec.Set) error {
	for _, name := range set.Names() {
		for _, d := range set.Namespaces[name].Filesystem.Writable {
			for _, p := range []string{cfg.StateDir, cfg.SecretsFile} {
				if isInside(d, p) || isInside(p, d) {
					return fmt.Errorf("namespace %s: writable path %s overlaps %s", name, d, p)
				}
			}
		}
	}
	return nil
}

// nsSet returns the current declarations.
func (s *Server) nsSet() *spec.Set { return s.setp.Load() }

// New loads the declarations and prepares the server.
func New(cfg *Config) (*Server, error) {
	set, err := spec.LoadDir(cfg.NamespacesDir, cfg.Home)
	if err != nil {
		return nil, err
	}
	if err := checkOverlap(cfg, set); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	admins := map[string]bool{}
	for _, a := range cfg.Admins {
		admins[a] = true
	}
	for _, d := range []string{cfg.PrivateDir(), cfg.PublicDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Server{
		cfg:     cfg,
		tokens:  auth.NewStore(cfg.TokensFile()),
		authz:   &auth.Authorizer{Admins: admins},
		secrets: secrets.File{Path: cfg.SecretsFile},
		log:     audit.New(cfg.AuditDir()),
		self:    self,
		goos:    runtime.GOOS,
		nss:     map[string]*nsRuntime{},
	}
	s.setp.Store(set)
	s.runs = newRuns(s)
	if err := s.runs.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetSecrets replaces the secret source (tests).
func (s *Server) SetSecrets(src secrets.Source) { s.secrets = src }

// Run starts the per-namespace proxies and gateways and serves the API until
// ctx is done.
func (s *Server) Run(ctx context.Context, apiListener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 16)
	s.mu.Lock()
	s.runCtx, s.errc = ctx, errc
	s.mu.Unlock()
	set := s.nsSet()
	for _, name := range set.Names() {
		if err := s.startNamespace(ctx, set.Namespaces[name], errc); err != nil {
			return fmt.Errorf("namespace %s: %w", name, err)
		}
	}
	go func() { errc <- s.serveAPI(ctx, apiListener) }()
	go func() {
		if err := s.serveControl(ctx); err != nil {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
		s.runs.stopAll()
		return nil
	case err := <-errc:
		s.runs.stopAll()
		return err
	}
}

func (s *Server) listen(name, kind string) (net.Listener, string, int, error) {
	if s.goos == "linux" {
		dir := filepath.Join(s.cfg.PrivateDir(), "sock", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", 0, err
		}
		p := filepath.Join(dir, kind+".sock")
		_ = os.Remove(p)
		l, err := net.Listen("unix", p)
		return l, p, 0, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", 0, err
	}
	return l, "", l.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) startNamespace(parent context.Context, ns *spec.Namespace, errc chan<- error) error {
	ctx, cancel := context.WithCancel(parent)
	rt := &nsRuntime{ns: ns, cancel: cancel}
	rt.proxy = egress.New(ns, s.secrets, s.log)
	if fn := s.cfg.upstreamFunc(); fn != nil {
		rt.proxy.SetUpstream(fn)
	}
	if s.proxyHook != nil {
		s.proxyHook(rt.proxy)
	}
	pl, psock, pport, err := s.listen(ns.Name, "proxy")
	if err != nil {
		cancel()
		return err
	}
	rt.proxySock, rt.proxyPort = psock, pport
	rt.gateway = mcpgw.New(ns, func(srv *spec.MCPServer) (*exec.Cmd, error) {
		return s.mcpCommand(rt, srv)
	}, s.log)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		pl.Close()
		cancel()
		return err
	}
	rt.proxy.GatewayKey = hex.EncodeToString(key)
	rt.gateway.SetRemote(s.proxyClient(rt), rt.proxy.GatewayKey)
	ml, msock, mport, err := s.listen(ns.Name, "mcp")
	if err != nil {
		pl.Close()
		cancel()
		return err
	}
	rt.mcpSock, rt.mcpPort = msock, mport
	// A listener that fails while its namespace is still wanted stops nsctl.
	report := func(err error) {
		if err != nil && ctx.Err() == nil {
			errc <- fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
	}
	go func() { report(rt.proxy.Serve(ctx, pl)) }()
	go func() { report(rt.gateway.Serve(ctx, ml)) }()
	s.mu.Lock()
	s.nss[ns.Name] = rt
	s.mu.Unlock()
	return nil
}

func (s *Server) runtime(name string) *nsRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nss[name]
}

// ReloadResult says what a reload changed.
type ReloadResult struct {
	Added, Updated, Removed []string
}

// Reload re-reads the declarations. If any file is invalid, nothing changes
// and the error is returned: a half-applied set is never served. Updated
// namespaces keep their endpoints (running agents stay connected) and get the
// new rules at once. Removed namespaces have their runs stopped.
func (s *Server) Reload() (ReloadResult, error) {
	var res ReloadResult
	set, err := spec.LoadDir(s.cfg.NamespacesDir, s.cfg.Home)
	if err != nil {
		return res, err
	}
	if err := checkOverlap(s.cfg, set); err != nil {
		return res, err
	}
	s.mu.Lock()
	ctx, errc := s.runCtx, s.errc
	s.mu.Unlock()
	if ctx == nil {
		return res, errors.New("not running")
	}
	old := s.nsSet()
	for _, name := range set.Names() {
		ns := set.Namespaces[name]
		if rt := s.runtime(name); rt != nil {
			s.mu.Lock()
			rt.ns = ns
			s.mu.Unlock()
			rt.proxy.SetNamespace(ns)
			rt.gateway.SetNamespace(ns)
			res.Updated = append(res.Updated, name)
			continue
		}
		if err := s.startNamespace(ctx, ns, errc); err != nil {
			return res, fmt.Errorf("namespace %s: %w", name, err)
		}
		res.Added = append(res.Added, name)
	}
	s.setp.Store(set)
	for _, name := range old.Names() {
		if set.Namespaces[name] != nil {
			continue
		}
		s.runs.stopNamespace(name)
		s.mu.Lock()
		rt := s.nss[name]
		delete(s.nss, name)
		s.mu.Unlock()
		if rt != nil {
			rt.cancel()
		}
		res.Removed = append(res.Removed, name)
	}
	s.log.Write(audit.Event{Kind: "reload", Allowed: true, Reason: fmt.Sprintf("added=%v updated=%v removed=%v", res.Added, res.Updated, res.Removed)})
	return res, nil
}

// proxyClient returns an HTTP client that sends every request through the
// namespace's own proxy (used by the MCP gateway for remote servers). The
// proxy address is read when the request is made.
func (s *Server) proxyClient(rt *nsRuntime) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Minute,
		// Never follow redirects: the request carries the gateway key, which
		// must only ever reach this namespace's proxy.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://nsctl-proxy.internal") },
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if rt.proxySock != "" {
					return d.DialContext(ctx, "unix", rt.proxySock)
				}
				return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(rt.proxyPort)))
			},
		},
	}
}

// current returns the runtime and its current declaration together.
func (s *Server) current(name string) (*nsRuntime, *spec.Namespace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.nss[name]
	if rt == nil {
		return nil, nil
	}
	return rt, rt.ns
}

// baseEnv is the only environment a sandboxed process gets from nsctl. The
// nsctl process environment is never passed through: it may hold secrets.
// term is the TERM value ("dumb" when there is no terminal).
func baseEnv(home string, ep sandbox.Endpoints, ns, term string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=" + getenvDefault("LANG", "C.UTF-8"),
		"TERM=" + term,
		"TMPDIR=/tmp",
		"NSCTL_NAMESPACE=" + ns,
	}
	if u := os.Getenv("USER"); u != "" {
		env = append(env, "USER="+u)
	}
	if ep.Proxy != "" {
		p := "http://" + ep.Proxy
		env = append(env,
			"HTTP_PROXY="+p, "HTTPS_PROXY="+p, "http_proxy="+p, "https_proxy="+p,
			"ALL_PROXY="+p, "all_proxy="+p, "NO_PROXY=", "no_proxy=")
	}
	return env
}

func getenvDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (s *Server) sandboxSpec(rt *nsRuntime, ns *spec.Namespace, withMCP bool) sandbox.Spec {
	sp := sandbox.Spec{
		Writable:    append([]string{}, ns.Filesystem.Writable...),
		DenyRead:    append(append([]string{}, ns.Filesystem.DenyRead...), s.cfg.AlwaysHidden()...),
		ProxySocket: rt.proxySock,
		ProxyPort:   rt.proxyPort,
		Self:        s.self,
		ReadFiles:   map[string]string{},
	}
	if withMCP {
		sp.MCPSocket, sp.MCPPort = rt.mcpSock, rt.mcpPort
	}
	return sp
}

func (s *Server) mcpCommand(rt *nsRuntime, srv *spec.MCPServer) (*exec.Cmd, error) {
	_, ns := s.current(rt.name())
	if ns == nil {
		return nil, errors.New("namespace removed")
	}
	sp := s.sandboxSpec(rt, ns, false)
	sp.Command = srv.Command
	sp.Workdir = "/tmp"
	ep := sp.Plan(s.goos)
	env := baseEnv(s.cfg.Home, ep, ns.Name, "dumb")
	keys := make([]string, 0, len(srv.Env))
	for k := range srv.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := srv.Env[k]
		val := v.Value
		if v.Secret != "" {
			var err error
			if val, err = s.secrets.Get(v.Secret); err != nil {
				return nil, fmt.Errorf("mcp server %s: %w", srv.Name, err)
			}
		}
		env = append(env, k+"="+val)
	}
	sp.Env = env
	cmd, err := sandbox.Command(sp)
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // server stderr is dropped; it may echo secrets
	return cmd, nil
}

// launch says how to start an agent: on one prompt, or in a person's
// terminal.
type launch struct {
	Prompt string
	// Terminal is set for interactive sessions (nsctl shell): the agent's
	// stdin, stdout and stderr, and the person's TERM.
	Terminal *terminal
}

type terminal struct {
	Stdin, Stdout, Stderr *os.File
	Term                  string
}

// argv is the agent's command line, before {mcp_config} is filled in.
func (l launch) argv(a *spec.Agent) []string {
	if l.Terminal != nil {
		return a.Interactive
	}
	out := make([]string, 0, len(a.Command))
	for _, arg := range a.Command {
		if arg == spec.PromptPlaceholder {
			arg = l.Prompt
		}
		out = append(out, arg)
	}
	return out
}

// agentCommand prepares a sandboxed agent run.
func (s *Server) agentCommand(rt *nsRuntime, ns *spec.Namespace, a *spec.Agent, l launch, runID string) (*exec.Cmd, error) {
	sp := s.sandboxSpec(rt, ns, len(ns.MCP.Servers) > 0)
	sp.Workdir = a.Workdir
	pubDir := filepath.Join(s.cfg.PublicDir(), runID)
	if err := os.MkdirAll(pubDir, 0o700); err != nil {
		return nil, err
	}
	mcpPath := filepath.Join(pubDir, "mcp.json")
	sp.ReadFiles["mcp.json"] = mcpPath
	ep := sp.Plan(s.goos)
	cfgJSON, err := mcpgw.ConfigJSON(ns, ep.MCP)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(mcpPath, cfgJSON, 0o600); err != nil {
		return nil, err
	}
	var argv []string
	for i, arg := range l.argv(a) {
		// The prompt is used as given; only the declared arguments are
		// templates.
		if l.Terminal == nil && a.Command[i] == spec.PromptPlaceholder {
			argv = append(argv, arg)
			continue
		}
		argv = append(argv, strings.ReplaceAll(arg, spec.MCPConfigPlaceholder, ep.Files["mcp.json"]))
	}
	sp.Command = argv
	home := s.cfg.Home
	if a.Home != "" {
		home = a.Home
	}
	term := "dumb"
	if l.Terminal != nil {
		term = l.Terminal.Term
	}
	env := baseEnv(home, ep, ns.Name, term)
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+a.Env[k])
	}
	sp.Env = env
	return sandbox.Command(sp)
}

// ErrNotFound is returned when a namespace, agent or run does not exist or is
// not visible to the caller.
var ErrNotFound = errors.New("not found")
