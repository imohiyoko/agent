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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/imohiyoko/agent/internal/egress"
	"github.com/imohiyoko/agent/internal/sandbox"
	"github.com/imohiyoko/agent/internal/secrets"
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
	t     *testing.T
	api   string
	srv   *Server
	state string
	work  string
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
filesystem:
  writable: [%q]
mcp:
  servers:
    - name: fake
      command: [%q, "fake-mcp"]
      env:
        FAKE_TAG: %s
      tools: [read_file]
agents:
  - name: sh
    command: ["sh", "-c", "{prompt}"]
    workdir: %q
`, name, member, role, upstream.URL, name, work, fake, name, work)
	}
	os.WriteFile(filepath.Join(nsDir, "a.yaml"), []byte(nsYAML("a", "alice", "operator")+"  - name: second\n    command: [\"true\"]\n    workdir: \""+work+"\"\n"), 0o600)
	os.WriteFile(filepath.Join(nsDir, "b.yaml"), []byte(nsYAML("b", "bob", "operator")), 0o600)
	cfg := &Config{NamespacesDir: nsDir, StateDir: filepath.Join(root, "state"), SecretsFile: filepath.Join(root, "secrets.yaml"), Home: home}
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
	return &env{t: t, api: "http://" + l.Addr().String(), srv: s, state: cfg.StateDir, work: work}
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
	if runtime.GOOS != "linux" {
		t.Skip("linux only (macOS needs a manual check)")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
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
		`echo "mcp=$(curl -s -X POST -H 'content-type: application/json' -H 'accept: application/json, text/event-stream' --noproxy '*' http://127.0.0.1:3129/mcp/fake -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')"`,
		`env | grep -c secret- | sed 's/^/envsecrets=/'`,
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
