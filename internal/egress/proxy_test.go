package egress

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

func setup(t *testing.T, ns *spec.Namespace) (*Proxy, *httptest.Server) {
	t.Helper()
	p := New(ns, secrets.Map{"t/key": "s3cret"}, audit.New(t.TempDir()))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

func client(proxyURL string) *http.Client {
	u, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

func connect(t *testing.T, proxyAddr, target string) string {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

func TestConnectDeniedByDefault(t *testing.T) {
	_, srv := setup(t, &spec.Namespace{Name: "t"})
	if got := connect(t, srv.Listener.Addr().String(), "example.com:443"); !strings.Contains(got, "403") {
		t.Fatalf("got %q, want 403", got)
	}
}

func TestConnectBlocksLoopbackEvenIfAllowed(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	var pn int
	fmt.Sscan(port, &pn)
	ns := &spec.Namespace{Name: "t", Network: spec.Network{Allow: []spec.HostRule{{Host: "localhost", Ports: []int{pn}}, {Host: "127.0.0.1", Ports: []int{pn}}}}}
	_, srv := setup(t, ns)
	for _, h := range []string{"localhost", "127.0.0.1"} {
		if got := connect(t, srv.Listener.Addr().String(), net.JoinHostPort(h, port)); !strings.Contains(got, "502") {
			t.Errorf("%s: got %q, want 502 (loopback refused)", h, got)
		}
	}
}

func TestConnectAllowed(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	var pn int
	fmt.Sscan(port, &pn)
	ns := &spec.Namespace{Name: "t", Network: spec.Network{Allow: []spec.HostRule{{Host: "127.0.0.1", Ports: []int{pn}}}}}
	p, srv := setup(t, ns)
	p.AllowLoopback = true
	if got := connect(t, srv.Listener.Addr().String(), "127.0.0.1:"+port); !strings.Contains(got, "200") {
		t.Fatalf("got %q, want 200", got)
	}
}

func TestCredentialRoute(t *testing.T) {
	var gotAuth, gotPath string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	ns := &spec.Namespace{Name: "t", Credentials: []spec.Credential{{
		Name: "gh", Upstream: up.URL + "/api", Header: "Authorization", Format: "Bearer {value}", Secret: "t/key",
		Rules: []spec.HTTPRule{{Methods: []string{"GET"}, Paths: []string{"/repos/me/**"}}},
	}}}
	p, srv := setup(t, ns)
	p.AllowLoopback = true
	p.Transport().TLSClientConfig = &tls.Config{RootCAs: up.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	c := client(srv.URL)

	// Allowed: header is added, agent's own header is replaced.
	req, _ := http.NewRequest("GET", "http://gh.creds.internal/repos/me/x", nil)
	req.Header.Set("Authorization", "Bearer placeholder")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || gotAuth != "Bearer s3cret" || gotPath != "/api/repos/me/x" {
		t.Fatalf("status %d auth %q path %q", resp.StatusCode, gotAuth, gotPath)
	}

	// Denied: wrong method, wrong path, traversal, unknown route.
	for _, c2 := range []struct{ method, url string }{
		{"POST", "http://gh.creds.internal/repos/me/x"},
		{"GET", "http://gh.creds.internal/repos/other/x"},
		{"GET", "http://gh.creds.internal/repos/me/../../admin"},
		{"GET", "http://nope.creds.internal/repos/me/x"},
	} {
		gotAuth = ""
		req, _ := http.NewRequest(c2.method, c2.url, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || gotAuth != "" {
			t.Errorf("%s %s: status %d (upstream saw auth %q)", c2.method, c2.url, resp.StatusCode, gotAuth)
		}
		if strings.Contains(string(b), "s3cret") {
			t.Errorf("secret leaked in the denial")
		}
	}
	// CONNECT to a credential route is refused (it must be plain HTTP).
	if got := connect(t, srv.Listener.Addr().String(), "gh.creds.internal:443"); !strings.Contains(got, "403") {
		t.Errorf("CONNECT to creds route: %q", got)
	}
}

func TestPlainHTTPDenied(t *testing.T) {
	_, srv := setup(t, &spec.Namespace{Name: "t"})
	resp, err := client(srv.URL).Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

// upstreamProxy is a minimal CONNECT proxy that records what it was asked.
func upstreamProxy(t *testing.T, seen *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.Host)
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		pipe(conn, up)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpstreamProxy(t *testing.T) {
	var seen []string
	upp := upstreamProxy(t, &seen)
	upURL, _ := url.Parse(upp.URL)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer target.Close()
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	var pn int
	fmt.Sscan(port, &pn)
	ns := &spec.Namespace{Name: "t",
		Network: spec.Network{Allow: []spec.HostRule{{Host: "127.0.0.1", Ports: []int{pn}}, {Host: "localhost", Ports: []int{pn}}}},
		Credentials: []spec.Credential{{Name: "gh", Upstream: target.URL, Header: "Authorization", Secret: "t/key",
			Rules: []spec.HTTPRule{{Methods: []string{"GET"}, Paths: []string{"/**"}}}}},
	}
	p, srv := setup(t, ns)
	p.SetUpstream(http.ProxyURL(upURL))
	p.Transport().TLSClientConfig = &tls.Config{RootCAs: target.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}

	// The guard still refuses loopback targets when an upstream proxy is used.
	if got := connect(t, srv.Listener.Addr().String(), "localhost:"+port); !strings.Contains(got, "502") {
		t.Errorf("loopback through upstream: %q", got)
	}
	if len(seen) != 0 {
		t.Errorf("upstream proxy was asked for a refused target: %v", seen)
	}

	p.AllowLoopback = true
	if got := connect(t, srv.Listener.Addr().String(), "127.0.0.1:"+port); !strings.Contains(got, "200") {
		t.Fatalf("CONNECT through upstream: %q", got)
	}
	resp, err := client(srv.URL).Get("http://gh.creds.internal/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "s3cret" {
		t.Errorf("credential route through upstream: %d %q", resp.StatusCode, b)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "CONNECT 127.0.0.1:") || !strings.HasPrefix(seen[1], "CONNECT 127.0.0.1:") {
		t.Errorf("upstream saw %v", seen)
	}
}

// The proxy also serves routes at /creds/<route>/ when called directly, with
// the same rules (for agents such as Gemini CLI that take plain http only on
// loopback).
func TestCredentialRouteByPath(t *testing.T) {
	var gotAuth, gotPath string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	rules := []spec.HTTPRule{{Methods: []string{"GET"}, Paths: []string{"/repos/me/**"}}}
	ns := &spec.Namespace{
		Name: "t",
		Credentials: []spec.Credential{
			{Name: "gh", Upstream: up.URL + "/api", Header: "Authorization", Format: "Bearer {value}", Secret: "t/key", Rules: rules},
			{Name: "gw", Upstream: up.URL, Header: "Authorization", Secret: "t/key", Rules: rules},
		},
		MCP: spec.MCP{Servers: []spec.MCPServer{{Name: "remote", Credential: "gw", Path: "/mcp", Tools: []string{"x"}}}},
	}
	p, srv := setup(t, ns)
	p.AllowLoopback = true
	p.Transport().TLSClientConfig = &tls.Config{RootCAs: up.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}

	resp, err := http.Get(srv.URL + "/creds/gh/repos/me/x?a=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || gotAuth != "Bearer s3cret" || gotPath != "/api/repos/me/x" {
		t.Fatalf("status %d auth %q path %q", resp.StatusCode, gotAuth, gotPath)
	}

	raw := func(path string) string {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", path)
		line, _ := bufio.NewReader(c).ReadString('\n')
		return strings.TrimSpace(line)
	}
	for _, path := range []string{
		"/creds/gh/repos/other/x",         // not in the rules
		"/creds/gh/repos/me/../../admin",  // traversal
		"/creds/gh/repos/me/%2e%2e/admin", // encoded traversal
		"/creds/nope/repos/me/x",          // no such route
		"/creds/gw/repos/me/x",            // only for the MCP gateway
		"/creds/",                         // no route
	} {
		gotAuth = ""
		if got := raw(path); !strings.Contains(got, "403") || gotAuth != "" {
			t.Errorf("%s: %q (upstream saw auth %q)", path, got, gotAuth)
		}
	}
	// Anything else on the proxy's own address is still refused.
	if got := raw("/other"); !strings.Contains(got, "400") {
		t.Errorf("/other: %q", got)
	}
}
