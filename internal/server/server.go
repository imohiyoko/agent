package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/auth"
	"github.com/imohiyoko/agent/internal/egress"
	"github.com/imohiyoko/agent/internal/mcpgw"
	"github.com/imohiyoko/agent/internal/sandbox"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

// nsRuntime is everything running for one namespace.
type nsRuntime struct {
	ns      *spec.Namespace
	proxy   *egress.Proxy
	gateway *mcpgw.Gateway
	// Linux: unix sockets. macOS: loopback ports.
	proxySock, mcpSock string
	proxyPort, mcpPort int
}

// Server is a running nsctl.
type Server struct {
	cfg     *Config
	set     *spec.Set
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
}

// New loads the declarations and prepares the server.
func New(cfg *Config) (*Server, error) {
	set, err := spec.LoadDir(cfg.NamespacesDir, cfg.Home)
	if err != nil {
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
		set:     set,
		tokens:  auth.NewStore(cfg.TokensFile()),
		authz:   &auth.Authorizer{Admins: admins},
		secrets: secrets.File{Path: cfg.SecretsFile},
		log:     audit.New(cfg.AuditDir()),
		self:    self,
		goos:    runtime.GOOS,
		nss:     map[string]*nsRuntime{},
	}
	s.runs = newRuns(s)
	return s, nil
}

// SetSecrets replaces the secret source (tests).
func (s *Server) SetSecrets(src secrets.Source) { s.secrets = src }

// Run starts the per-namespace proxies and gateways and serves the API until
// ctx is done.
func (s *Server) Run(ctx context.Context, apiListener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2*len(s.set.Namespaces)+1)
	for _, name := range s.set.Names() {
		if err := s.startNamespace(ctx, s.set.Namespaces[name], errc); err != nil {
			return fmt.Errorf("namespace %s: %w", name, err)
		}
	}
	go func() { errc <- s.serveAPI(ctx, apiListener) }()
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

func (s *Server) startNamespace(ctx context.Context, ns *spec.Namespace, errc chan<- error) error {
	rt := &nsRuntime{ns: ns}
	rt.proxy = egress.New(ns, s.secrets, s.log)
	if s.proxyHook != nil {
		s.proxyHook(rt.proxy)
	}
	pl, psock, pport, err := s.listen(ns.Name, "proxy")
	if err != nil {
		return err
	}
	rt.proxySock, rt.proxyPort = psock, pport
	rt.gateway = mcpgw.New(ns, func(srv *spec.MCPServer) (*exec.Cmd, error) {
		return s.mcpCommand(rt, srv)
	}, s.log)
	ml, msock, mport, err := s.listen(ns.Name, "mcp")
	if err != nil {
		pl.Close()
		return err
	}
	rt.mcpSock, rt.mcpPort = msock, mport
	go func() { errc <- rt.proxy.Serve(ctx, pl) }()
	go func() { errc <- rt.gateway.Serve(ctx, ml) }()
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

// baseEnv is the only environment a sandboxed process gets from nsctl. The
// nsctl process environment is never passed through: it may hold secrets.
func baseEnv(home string, ep sandbox.Endpoints, ns string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=" + getenvDefault("LANG", "C.UTF-8"),
		"TERM=dumb",
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

func (s *Server) sandboxSpec(rt *nsRuntime, withMCP bool) sandbox.Spec {
	sp := sandbox.Spec{
		Writable:    append([]string{}, rt.ns.Filesystem.Writable...),
		DenyRead:    append(append([]string{}, rt.ns.Filesystem.DenyRead...), s.cfg.AlwaysHidden()...),
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
	sp := s.sandboxSpec(rt, false)
	sp.Command = srv.Command
	sp.Workdir = "/tmp"
	ep := sp.Plan(s.goos)
	env := baseEnv(s.cfg.Home, ep, rt.ns.Name)
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

// agentCommand prepares a sandboxed agent run.
func (s *Server) agentCommand(rt *nsRuntime, a *spec.Agent, prompt, runID string) (*exec.Cmd, error) {
	sp := s.sandboxSpec(rt, len(rt.ns.MCP.Servers) > 0)
	sp.Workdir = a.Workdir
	pubDir := filepath.Join(s.cfg.PublicDir(), runID)
	if err := os.MkdirAll(pubDir, 0o700); err != nil {
		return nil, err
	}
	mcpPath := filepath.Join(pubDir, "mcp.json")
	sp.ReadFiles["mcp.json"] = mcpPath
	ep := sp.Plan(s.goos)
	cfgJSON, err := mcpgw.ConfigJSON(rt.ns, ep.MCP)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(mcpPath, cfgJSON, 0o600); err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(a.Command))
	for _, arg := range a.Command {
		switch {
		case arg == spec.PromptPlaceholder:
			argv = append(argv, prompt)
		default:
			argv = append(argv, strings.ReplaceAll(arg, spec.MCPConfigPlaceholder, ep.Files["mcp.json"]))
		}
	}
	sp.Command = argv
	env := baseEnv(s.cfg.Home, ep, rt.ns.Name)
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
