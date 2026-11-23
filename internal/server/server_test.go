package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imohiyoko/agent/internal/egress"
	"github.com/imohiyoko/agent/internal/mcpgw"
	"github.com/imohiyoko/agent/internal/sandbox"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

// The test binary doubles as nsctl (sandbox-init) and as a fake MCP server.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-init" {
		args := os.Args[2:]
		var fwd []string
		for len(args) > 0 && args[0] != "--" {
			fwd = append(fwd, args[1])
			args = args[2:]
		}
		os.Exit(sandbox.Init(fwd, args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-mcp" {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			var msg map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg["id"] == nil {
				continue
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"fake-%s"}}}`+"\n", msg["id"], os.Getenv("FAKE_TAG"))
		}
		return
	}
	os.Exit(m.Run())
}

type env struct {
	t      *testing.T
	api    string
	srv    *Server
	state  string
	work   string
	nsDir  string
	cfg    *Config
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *env) call(token, method, path, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.api+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func start(t *testing.T, upstream *httptest.Server) *env {
	t.Helper()
	home, _ := os.UserHomeDir()
	// Under the home directory (not /tmp, which the sandbox replaces), so the
	// tests show that hiding works.
	root, err := os.MkdirTemp(home, ".nsctl-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	nsDir := filepath.Join(root, "namespaces")
	for _, d := range []string{work, nsDir} {
		os.MkdirAll(d, 0o700)
	}
	// Copy the test binary where the MCP server's sandbox can see it.
	self, _ := os.Executable()
	b, _ := os.ReadFile(self)
	fake := filepath.Join(work, "fake-mcp")
	os.WriteFile(fake, b, 0o700)

	nsYAML := func(name, member, role string) string {
		return fmt.Sprintf(`version: 1
name: %s
members:
  - principal: %s
    role: %s
credentials:
  - name: up
    upstream: %s
    header: Authorization
    format: "Bearer {value}"
    secret: %s/key
    rules:
      - methods: [GET]
        paths: ["/ok"]
  - name: mcpup
    upstream: %s
    header: Authorization
    format: "Bearer {value}"
    secret: %s/key
    rules:
      - methods: [POST, DELETE]
        paths: ["/mcp"]
filesystem:
  writable: [%q]
mcp:
  servers:
    - name: fake
      command: [%q, "fake-mcp"]
      env:
        FAKE_TAG: %s
      tools: [read_file]
    - name: remote
      credential: mcpup
      tools: [read_file]
agents:
  - name: sh
    command: ["sh", "-c", "{prompt}"]
    workdir: %q
    home: %q
    env:
      FAKE_MCP: "{mcp_url}/fake"
      UP_BASE: "{creds_url}/up"
`, name, member, role, upstream.URL, name, upstream.URL, name, work, fake, name, work, work+"/home")
	}
	extra := fmt.Sprintf(`  - name: second
    command: ["true"]
    workdir: %q
  - name: sleepy
    command: ["sleep", "30"]
    workdir: %q
    timeout: 1s
  - name: loud
    command: ["sh", "-c", "head -c 5000 /dev/zero | tr '\\0' x"]
    workdir: %q
  - name: talk
    command: ["true"]
    interactive: ["sh", "-c", "read x; echo \"got=$x term=$TERM\"; exit 3"]
    workdir: %q
  - name: wait
    command: ["true"]
    interactive: ["sleep", "30"]
    workdir: %q
`, work, work, work, work, work)
	os.WriteFile(filepath.Join(nsDir, "a.yaml"), []byte(nsYAML("a", "alice", "operator")+extra), 0o600)
	os.WriteFile(filepath.Join(nsDir, "b.yaml"), []byte(nsYAML("b", "bob", "operator")), 0o600)
	cfg := &Config{NamespacesDir: nsDir, StateDir: filepath.Join(root, "state"), SecretsFile: filepath.Join(root, "secrets.yaml"), Home: home, MaxLogBytes: 2048}
	cfg.resolve(root)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSecrets(secrets.Map{"a/key": "secret-a", "b/key": "secret-b"})
	pool := upstream.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	s.proxyHook = func(p *egress.Proxy) {
		p.AllowLoopback = true
		p.Transport().TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(100 * time.Millisecond)
	return &env{t: t, api: "http://" + l.Addr().String(), srv: s, state: cfg.StateDir, work: work, nsDir: nsDir, cfg: cfg, cancel: cancel, done: done}
}

func needSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("linux only (macOS needs a manual check)")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
}

func (e *env) run(token, agent, prompt string) Run {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"agent": agent, "prompt": prompt})
	code, resp := e.call(token, "POST", "/v1/namespaces/a/runs", string(body))
	if code != 201 {
		e.t.Fatalf("create run: %d %s", code, resp)
	}
	var run Run
	json.Unmarshal([]byte(resp), &run)
	return run
}

func TestRunTimeoutLogCapAndPersistence(t *testing.T) {
	needSandbox(t)
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	e := start(t, up)
	_, alice, _ := e.srv.Tokens().Create("alice", "")

	slow := e.run(alice, "sleepy", "")
	loud := e.run(alice, "loud", "")
	e.srv.runs.wait(slow.ID)
	e.srv.runs.wait(loud.ID)
	if got := e.srv.runs.get("a", slow.ID); got.State != StateTimeout {
		t.Errorf("sleepy state %q, want timeout", got.State)
	}
	_, out := e.call(alice, "GET", "/v1/namespaces/a/runs/"+loud.ID+"/logs", "")
	if !strings.Contains(out, "output truncated") || len(out) > 2048+100 {
		t.Errorf("log not capped: %d bytes", len(out))
	}

	// A new server over the same state sees the old runs; a run left
	// "running" in the record is shown as lost.
	rec := filepath.Join(e.state, "private", "runs", slow.ID, "run.json")
	b, _ := os.ReadFile(rec)
	os.WriteFile(rec, []byte(strings.Replace(string(b), `"state": "timeout"`, `"state": "running"`, 1)), 0o600)
	s2, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.runs.get("a", slow.ID); got == nil || got.State != StateLost {
		t.Errorf("after restart: %+v", got)
	}
	if got := s2.runs.get("a", loud.ID); got == nil || got.State != StateExited {
		t.Errorf("after restart: %+v", got)
	}
}

func TestLogsFollow(t *testing.T) {
	needSandbox(t)
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	e := start(t, up)
	_, alice, _ := e.srv.Tokens().Create("alice", "")
	run := e.run(alice, "sh", "echo first; sleep 1; echo second")
	code, out := e.call(alice, "GET", "/v1/namespaces/a/runs/"+run.ID+"/logs?follow=1", "")
	if code != 200 || !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("follow: %d %q", code, out)
	}
}

func TestReload(t *testing.T) {
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	e := start(t, up)
	_, bob, _ := e.srv.Tokens().Create("bob", "")
	_, carol, _ := e.srv.Tokens().Create("carol", "")

	// An invalid file leaves everything as it was.
	os.WriteFile(filepath.Join(e.nsDir, "c.yaml"), []byte("version: 1\nname: c\nnetwork:\n  allow:\n    - host: \"*\"\n      ports: [443]\n"), 0o600)
	if _, err := e.srv.Reload(); err == nil {
		t.Fatal("invalid declaration was accepted")
	}
	if code, _ := e.call(bob, "GET", "/v1/namespaces/b", ""); code != 200 {
		t.Fatalf("b gone after a failed reload: %d", code)
	}

	// Remove b, add c with carol.
	os.Remove(filepath.Join(e.nsDir, "b.yaml"))
	os.WriteFile(filepath.Join(e.nsDir, "c.yaml"), []byte("version: 1\nname: c\nmembers:\n  - principal: carol\n    role: viewer\n"), 0o600)
	res, err := e.srv.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(res.Added) != "[c]" || fmt.Sprint(res.Removed) != "[b]" {
		t.Errorf("reload result %+v", res)
	}
	if code, _ := e.call(bob, "GET", "/v1/namespaces/b", ""); code != 404 {
		t.Errorf("removed namespace still served: %d", code)
	}
	if code, _ := e.call(carol, "GET", "/v1/namespaces/c", ""); code != 200 {
		t.Errorf("added namespace not served: %d", code)
	}
	if e.srv.runtime("b") != nil || e.srv.runtime("c") == nil {
		t.Error("runtimes not updated")
	}
}

func TestAPIAuth(t *testing.T) {
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	e := start(t, up)
	_, alice, _ := e.srv.Tokens().Create("alice", "")
	_, bob, _ := e.srv.Tokens().Create("bob", "")
	_, carol, _ := e.srv.Tokens().Create("carol", "")

	if code, _ := e.call("", "GET", "/v1/namespaces", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.call("nsk_wrong", "GET", "/v1/namespaces", ""); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	if _, body := e.call(alice, "GET", "/v1/namespaces", ""); !strings.Contains(body, `"name": "a"`) || strings.Contains(body, `"name": "b"`) {
		t.Errorf("alice sees: %s", body)
	}
	if _, body := e.call(carol, "GET", "/v1/namespaces", ""); strings.TrimSpace(body) != "[]" {
		t.Errorf("carol sees: %s", body)
	}
	// A namespace you cannot read looks like one that does not exist.
	if code, _ := e.call(bob, "GET", "/v1/namespaces/a", ""); code != 404 {
		t.Errorf("bob reads a: %d", code)
	}
	if code, _ := e.call(bob, "POST", "/v1/namespaces/a/runs", `{"agent":"sh","prompt":"true"}`); code != 404 {
		t.Errorf("bob runs in a: %d", code)
	}
	// Operators cannot read the audit log.
	if code, _ := e.call(alice, "GET", "/v1/namespaces/a/audit", ""); code != 403 {
		t.Errorf("alice audit: %d", code)
	}
	// Only declared agents can be started.
	if code, _ := e.call(alice, "POST", "/v1/namespaces/a/runs", `{"agent":"rm","prompt":"x"}`); code != 400 {
		t.Errorf("undeclared agent: %d", code)
	}
	if code, _ := e.call(alice, "POST", "/v1/namespaces/a/runs", `{"agent":"sh","prompt":"x","command":"rm"}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
}

// TestRunInSandbox runs a real agent run through bwrap: the credential route
// works, everything else is blocked, private files are hidden and the MCP
// gateway answers with this namespace's server.
func TestRunInSandbox(t *testing.T) {
	needSandbox(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	var seen string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		io.WriteString(w, "upstream-ok")
	}))
	defer up.Close()
	e := start(t, up)
	_, alice, _ := e.srv.Tokens().Create("alice", "")

	script := strings.Join([]string{
		`echo "cred=$(curl -s http://up.creds.internal/ok)"`,
		`echo "credpost=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://up.creds.internal/ok)"`,
		`echo "connect=$(curl -s -o /dev/null -w '%{http_connect}' --max-time 5 https://example.com/)"`,
		`curl -s --noproxy '*' --max-time 3 http://1.1.1.1/ >/dev/null 2>&1 && echo direct=OPEN || echo direct=blocked`,
		`ls ` + e.state + `/private 2>/dev/null | grep -q tokens && echo private=VISIBLE || echo private=hidden`,
		// No --noproxy: NO_PROXY must already send loopback (the gateway) direct.
		`echo "mcp=$(curl -s -X POST -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' "$NSCTL_MCP_URL/fake" -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')"`,
		`echo "mcpenv=$FAKE_MCP"`,
		`echo "credpath=$(curl -s "$UP_BASE/ok")"`,
		`env | grep -c secret- | sed 's/^/envsecrets=/'`,
		`echo "home=$HOME"`,
	}, "; ")
	body, _ := json.Marshal(map[string]string{"agent": "sh", "prompt": script})
	code, resp := e.call(alice, "POST", "/v1/namespaces/a/runs", string(body))
	if code != 201 {
		t.Fatalf("create run: %d %s", code, resp)
	}
	var run Run
	json.Unmarshal([]byte(resp), &run)
	e.srv.runs.wait(run.ID)
	_, out := e.call(alice, "GET", "/v1/namespaces/a/runs/"+run.ID+"/logs", "")
	if strings.Contains(out, "No permissions to create new namespace") {
		t.Skip("user namespaces not available")
	}
	for _, want := range []string{
		"cred=upstream-ok",
		"credpost=403",
		"connect=403",
		"direct=blocked",
		"private=hidden",
		`"name":"fake-a"`,
		"envsecrets=0",
		"home=" + e.work + "/home",
		"mcpenv=http://127.0.0.1:3129/mcp/fake",
		"credpath=upstream-ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if seen != "Bearer secret-a" {
		t.Errorf("upstream saw %q, want namespace a's secret", seen)
	}
	_, st := e.call(alice, "GET", "/v1/namespaces/a/runs/"+run.ID, "")
	if !strings.Contains(st, `"state": "exited"`) {
		t.Errorf("run state: %s", st)
	}
}

func TestDoctorAndOverlap(t *testing.T) {
	root := t.TempDir()
	nsDir := filepath.Join(root, "namespaces")
	os.MkdirAll(nsDir, 0o700)
	state := filepath.Join(root, "state")
	os.WriteFile(filepath.Join(nsDir, "a.yaml"), []byte(fmt.Sprintf(`version: 1
name: a
credentials:
  - name: up
    upstream: https://example.com
    header: X-Key
    secret: a/missing
    rules:
      - methods: [GET]
        paths: ["/"]
filesystem:
  writable: [%q]
agents:
  - name: x
    command: ["no-such-command-xyz"]
    workdir: %q
`, root, root)), 0o600)
	cfg := &Config{NamespacesDir: nsDir, StateDir: state, SecretsFile: filepath.Join(root, "secrets.yaml"), Home: "/nonexistent-home", Listen: "127.0.0.1:7070"}
	cfg.resolve(root)
	os.WriteFile(cfg.SecretsFile, []byte("a/other: x\n"), 0o644)

	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("writable path over the state directory was accepted: %v", err)
	}
	var out strings.Builder
	n := Doctor(cfg, &out)
	for _, want := range []string{"secret a/missing", "no-such-command-xyz", "overlaps the state directory", "secrets " + cfg.SecretsFile} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out.String())
		}
	}
	if n == 0 {
		t.Error("doctor found no problems")
	}
	if strings.Contains(out.String(), "a/other: x") {
		t.Error("doctor printed a secret value")
	}
}

// fakeRemoteMCP answers MCP over Streamable HTTP at /mcp. It answers
// tools/list with SSE and everything else with JSON, and records calls.
func fakeRemoteMCP(calls *[]string, auth *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		if r.Header.Get(egress.GatewayHeader) != "" {
			http.Error(w, "gateway key leaked upstream", 500)
			return
		}
		var m map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&m)
		var method string
		json.Unmarshal(m["method"], &method)
		*calls = append(*calls, method)
		if method != "initialize" && r.Header.Get("Mcp-Session-Id") != "remote-session" {
			http.Error(w, "missing session", http.StatusNotFound)
			return
		}
		w.Header().Set("Mcp-Session-Id", "remote-session")
		switch method {
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":99,\"method\":\"sampling/createMessage\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[{\"name\":\"read_file\"},{\"name\":\"delete_file\"}]}}\n\n", m["id"])
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, m["id"])
		}
	}
}

func unixOrTCPClient(sock string, port int) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if sock != "" {
			return net.Dial("unix", sock)
		}
		return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}}}
}

func TestRemoteMCP(t *testing.T) {
	if mcpgwHeader() != egress.GatewayHeader {
		t.Fatal("mcpgw.GatewayHeader and egress.GatewayHeader differ")
	}
	var calls []string
	var auth string
	up := httptest.NewTLSServer(fakeRemoteMCP(&calls, &auth))
	defer up.Close()
	e := start(t, up)
	rt := e.srv.runtime("a")
	gw := unixOrTCPClient(rt.mcpSock, rt.mcpPort)
	sid := ""
	post := func(body string) (string, http.Header) {
		req, _ := http.NewRequest("POST", "http://gw/mcp/remote", strings.NewReader(body))
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := gw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.Header
	}
	out, h := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if !strings.Contains(out, `"ok":true`) || h.Get("Mcp-Session-Id") != "remote-session" {
		t.Fatalf("initialize: %q %v", out, h)
	}
	sid = h.Get("Mcp-Session-Id")
	if auth != "Bearer secret-a" {
		t.Errorf("upstream saw %q", auth)
	}
	out, _ = post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(out, "read_file") || strings.Contains(out, "delete_file") || strings.Contains(out, "sampling") {
		t.Errorf("tools/list not filtered: %q", out)
	}
	n := len(calls)
	out, _ = post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delete_file"}}`)
	if !strings.Contains(out, "not allowed") || len(calls) != n {
		t.Errorf("denied tool reached the server: %q %v", out, calls)
	}

	// The agent cannot skip the gateway by calling the route itself.
	px := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy") },
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if rt.proxySock != "" {
				return net.Dial("unix", rt.proxySock)
			}
			return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", rt.proxyPort))
		},
	}}
	for _, key := range []string{"", "guess"} {
		req, _ := http.NewRequest("POST", "http://mcpup.creds.internal/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_file"}}`))
		if key != "" {
			req.Header.Set(egress.GatewayHeader, key)
		}
		resp, err := px.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("direct use of a gateway-only route (key %q): %d", key, resp.StatusCode)
		}
	}
	if len(calls) != n {
		t.Errorf("direct call reached the server: %v", calls)
	}
}

func mcpgwHeader() string { return mcpgw.GatewayHeader }

func TestClaudeAuthProblems(t *testing.T) {
	ns := &spec.Namespace{Name: "a", Credentials: []spec.Credential{{Name: "claude"}}}
	cases := []struct {
		env  map[string]string
		want string // "" = no problem
	}{
		{map[string]string{"ANTHROPIC_BASE_URL": "http://claude.creds.internal", "CLAUDE_CODE_OAUTH_TOKEN": "p"}, ""},
		{map[string]string{"ANTHROPIC_BASE_URL": "http://claude.creds.internal/", "ANTHROPIC_API_KEY": "p"}, ""},
		{map[string]string{"ANTHROPIC_BASE_URL": "{creds_url}/claude", "ANTHROPIC_API_KEY": "p"}, ""},
		{map[string]string{"ANTHROPIC_BASE_URL": "{creds_url}/other", "ANTHROPIC_API_KEY": "p"}, "not a credential route"},
		{map[string]string{"ANTHROPIC_BASE_URL": "http://claude.creds.internal", "ANTHROPIC_API_KEY": "p", "CLAUDE_CODE_OAUTH_TOKEN": "p"}, "set only one"},
		{map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "p"}, "ANTHROPIC_BASE_URL is not set"},
		{map[string]string{"ANTHROPIC_BASE_URL": "http://other.creds.internal", "CLAUDE_CODE_OAUTH_TOKEN": "p"}, "not a credential route"},
		{map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com", "CLAUDE_CODE_OAUTH_TOKEN": "p"}, "not a credential route"},
		{map[string]string{}, ""},
	}
	for _, c := range cases {
		got := strings.Join(claudeAuthProblems(ns, &spec.Agent{Env: c.env}), "; ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("env %v: got %q, want %q", c.env, got, c.want)
		}
	}
}

func TestShell(t *testing.T) {
	needSandbox(t)
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	e := start(t, up)
	pipe := func() (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		return r, w
	}

	// The agent reads the person's input and writes to their terminal.
	inR, inW := pipe()
	outR, outW := pipe()
	type result struct {
		res ShellResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := Shell(e.cfg, "a", "talk", "xterm-test", inR, outW, outW, nil)
		done <- result{res, err}
	}()
	io.WriteString(inW, "hello\n")
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	outW.Close()
	out, _ := io.ReadAll(outR)
	if strings.Contains(string(out), "No permissions to create new namespace") {
		t.Skip("user namespaces not available")
	}
	if !strings.Contains(string(out), "got=hello term=xterm-test") {
		t.Fatalf("agent output %q", out)
	}
	if got.res.ExitCode != 3 || got.res.State != StateFailed {
		t.Errorf("result %+v, want exit 3, state failed", got.res)
	}
	run := e.srv.runs.get("a", got.res.Run)
	if run == nil || run.Mode != ModeInteractive || run.Principal != LocalPrincipal || run.Deadline != nil {
		t.Fatalf("run record %+v", run)
	}
	if log, _ := os.ReadFile(run.logPath); strings.Contains(string(log), "got=") {
		t.Error("an interactive session's output was recorded")
	}

	// An agent without an interactive command is refused.
	_, outW = pipe()
	if _, err := Shell(e.cfg, "a", "sh", "", inR, outW, outW, nil); err == nil || !strings.Contains(err.Error(), "no interactive command") {
		t.Errorf("agent without interactive: %v", err)
	}
	if _, err := Shell(e.cfg, "zzz", "talk", "", inR, outW, outW, nil); err == nil {
		t.Error("unknown namespace accepted")
	}

	// Closing the terminal side (the client) stops the session.
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: e.cfg.ControlSocket(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	rights := syscall.UnixRights(int(inR.Fd()), int(outW.Fd()), int(outW.Fd()))
	if _, _, err := conn.WriteMsgUnix([]byte(`{"namespace":"a","agent":"wait"}`), rights, nil); err != nil {
		t.Fatal(err)
	}
	var first shellReply
	if err := json.NewDecoder(conn).Decode(&first); err != nil || first.Run == "" {
		t.Fatalf("first reply %+v, %v", first, err)
	}
	conn.Close()
	select {
	case <-e.srv.runs.doneCh("a", first.Run):
	case <-time.After(15 * time.Second):
		t.Fatal("session kept running after the client went away")
	}
	if r := e.srv.runs.get("a", first.Run); r.State != StateStopped {
		t.Errorf("state %s, want stopped", r.State)
	}
}

func TestRouteProblems(t *testing.T) {
	ns := &spec.Namespace{Name: "a", Credentials: []spec.Credential{{Name: "openai"}}}
	got := strings.Join(routeProblems(ns, &spec.Agent{Env: map[string]string{
		"OPENAI_BASE_URL":        "http://openai.creds.internal/v1",
		"GOOGLE_GEMINI_BASE_URL": "http://gemini.creds.internal",
		"X_BASE_URL":             "{creds_url}/missing",
		"Y_BASE_URL":             "{creds_url}/openai",
		"Z_BASE_URL":             "{creds_url}/openai_backup/v1",
		"OTHER":                  "https://openai.creds.internal/v1",
	}}), "; ")
	for _, want := range []string{`route "gemini"`, "OTHER uses https://", `route "missing"`, `route "openai_backup"`} {
		if !strings.Contains(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "OPENAI_BASE_URL") || strings.Contains(got, "Y_BASE_URL") {
		t.Errorf("a declared route was reported: %q", got)
	}
}
