// Package egress is the per-namespace HTTP proxy. It is the only way out of
// a namespace's sandbox (ADR 0002).
//
//   - CONNECT host:port is tunnelled only if network.allow permits it. TLS is
//     not broken, so HTTPS cannot be filtered by path.
//   - Plain HTTP to <name>.creds.internal is a credential route: the method
//     and path are checked, the credential header is added, and the request
//     is sent to the upstream over TLS. The agent never sees the value.
//   - Plain HTTP to other hosts is forwarded if network.allow permits it.
//   - Connections to loopback, link-local, multicast and unspecified
//     addresses are refused after DNS resolution, whatever the rules say, so
//     an allowed name that resolves to 127.0.0.1 cannot reach nsctl itself.
package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

// Proxy serves one namespace.
type Proxy struct {
	ns      atomic.Pointer[spec.Namespace]
	secrets secrets.Source
	log     *audit.Log
	dialer  *net.Dialer
	plain   *net.Dialer
	tr      *http.Transport

	// upstream, if set, picks the HTTP proxy nsctl itself must use for a URL
	// (for networks where nsctl cannot connect out directly).
	upstream func(*http.Request) (*url.URL, error)

	// AllowLoopback is for tests only: it lets the proxy reach test servers
	// on 127.0.0.1.
	AllowLoopback bool
}

// New returns a proxy for ns.
func New(ns *spec.Namespace, src secrets.Source, log *audit.Log) *Proxy {
	p := &Proxy{secrets: src, log: log}
	p.ns.Store(ns)
	p.dialer = &net.Dialer{Timeout: 15 * time.Second, Control: p.checkAddr}
	p.plain = &net.Dialer{Timeout: 15 * time.Second}
	p.tr = &http.Transport{
		// Only the upstream set with SetUpstream, never the environment.
		Proxy: func(r *http.Request) (*url.URL, error) {
			if p.upstream == nil {
				return nil, nil
			}
			return p.upstream(r)
		},
		DialContext:           p.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return p
}

// Name is the namespace's name (it never changes).
func (p *Proxy) Name() string { return p.ns.Load().Name }

// SetNamespace replaces the rules (for reloads).
func (p *Proxy) SetNamespace(ns *spec.Namespace) { p.ns.Store(ns) }

// SetUpstream makes nsctl reach the internet through an HTTP proxy. fn
// returns the proxy for a URL, or nil to connect directly (for example
// http.ProxyFromEnvironment). Call it before serving.
func (p *Proxy) SetUpstream(fn func(*http.Request) (*url.URL, error)) { p.upstream = fn }

func (p *Proxy) upstreamFor(scheme, hostport string) *url.URL {
	if p.upstream == nil {
		return nil
	}
	u, err := p.upstream(&http.Request{URL: &url.URL{Scheme: scheme, Host: hostport}})
	if err != nil {
		return nil
	}
	return u
}

// dial connects to addr. The upstream proxy itself may be on loopback; every
// other address goes through the guarded dialer.
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.upstream != nil && p.isUpstreamAddr(addr) {
		return p.plain.DialContext(ctx, network, addr)
	}
	return p.dialer.DialContext(ctx, network, addr)
}

func (p *Proxy) isUpstreamAddr(addr string) bool {
	for _, scheme := range []string{"https", "http"} {
		if u := p.upstreamFor(scheme, "example.invalid:443"); u != nil && hostPortOf(u) == addr {
			return true
		}
	}
	return false
}

func hostPortOf(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// checkHost applies the address guard by name, for connections that an
// upstream proxy resolves. It resolves the name itself; a name that changes
// its answer between this check and the proxy's own lookup is not caught.
func (p *Proxy) checkHost(ctx context.Context, host string) error {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if p.AllowLoopback {
			return nil
		}
		return fmt.Errorf("%s is not reachable from a namespace", host)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if err := p.checkAddr("tcp", net.JoinHostPort(ip.IP.String(), "0"), nil); err != nil {
			return err
		}
	}
	return nil
}

// dialTarget opens a TCP connection to hostname:port for a CONNECT tunnel,
// through the upstream proxy when one applies.
func (p *Proxy) dialTarget(ctx context.Context, hostname string, port int) (net.Conn, error) {
	target := net.JoinHostPort(hostname, strconv.Itoa(port))
	up := p.upstreamFor("https", target)
	if up == nil {
		return p.dialer.DialContext(ctx, "tcp", target)
	}
	if err := p.checkHost(ctx, hostname); err != nil {
		return nil, err
	}
	c, err := p.plain.DialContext(ctx, "tcp", hostPortOf(up))
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("upstream proxy: %s", resp.Status)
	}
	_ = c.SetReadDeadline(time.Time{})
	if br.Buffered() > 0 {
		c.Close()
		return nil, fmt.Errorf("upstream proxy sent data early")
	}
	return c, nil
}

// Transport is exposed for tests that need to trust a test TLS server.
func (p *Proxy) Transport() *http.Transport { return p.tr }

func (p *Proxy) checkAddr(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("not an IP: %s", host)
	}
	if ip.IsLoopback() && p.AllowLoopback {
		return nil
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("address %s is not reachable from a namespace", ip)
	}
	return nil
}

func (p *Proxy) record(kind, action, target string, allowed bool, reason string) {
	p.log.Write(audit.Event{
		Namespace: p.ns.Load().Name, Kind: kind, Action: action,
		Target: target, Allowed: allowed, Reason: reason,
	})
}

func deny(w http.ResponseWriter, code int, reason string) {
	http.Error(w, "nsctl: denied: "+reason, code)
}

// ServeHTTP implements the proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	host := r.URL.Host
	if host == "" {
		host = r.Host
	}
	hostname, port := splitHostPort(host, 80)
	if name, ok := strings.CutSuffix(hostname, spec.CredsSuffix); ok {
		p.credential(w, r, name)
		return
	}
	if r.URL.Scheme != "http" {
		p.record("egress", r.Method, host, false, "not a proxy request")
		deny(w, http.StatusBadRequest, "send requests through the proxy (absolute URL) or use CONNECT")
		return
	}
	if !p.ns.Load().Network.AllowHost(hostname, port) {
		p.record("egress", r.Method, net.JoinHostPort(hostname, strconv.Itoa(port)), false, "not in network.allow")
		deny(w, http.StatusForbidden, hostname+" is not allowed in this namespace")
		return
	}
	if p.upstreamFor("http", host) != nil {
		if err := p.checkHost(r.Context(), hostname); err != nil {
			p.record("egress", r.Method, host, false, err.Error())
			deny(w, http.StatusBadGateway, "cannot connect")
			return
		}
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	removeHopHeaders(out.Header)
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.record("egress", r.Method, host, false, err.Error())
		deny(w, http.StatusBadGateway, "upstream error")
		return
	}
	p.record("egress", r.Method, host, true, "")
	copyResponse(w, resp)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	hostname, port := splitHostPort(r.Host, 443)
	target := net.JoinHostPort(hostname, strconv.Itoa(port))
	if strings.HasSuffix(hostname, spec.CredsSuffix) {
		p.record("egress", "CONNECT", target, false, "credential routes take plain HTTP")
		deny(w, http.StatusForbidden, "use http:// for credential routes")
		return
	}
	if !p.ns.Load().Network.AllowHost(hostname, port) {
		p.record("egress", "CONNECT", target, false, "not in network.allow")
		deny(w, http.StatusForbidden, hostname+" is not allowed in this namespace")
		return
	}
	up, err := p.dialTarget(r.Context(), hostname, port)
	if err != nil {
		p.record("egress", "CONNECT", target, false, err.Error())
		deny(w, http.StatusBadGateway, "cannot connect")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		deny(w, http.StatusInternalServerError, "cannot tunnel")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	p.record("egress", "CONNECT", target, true, "")
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		_, _ = up.Write(b)
	}
	pipe(conn, up)
}

func (p *Proxy) credential(w http.ResponseWriter, r *http.Request, name string) {
	ns := p.ns.Load()
	c := ns.Credential(name)
	target := name + spec.CredsSuffix + r.URL.Path
	if c == nil {
		p.record("credential", r.Method, target, false, "no such credential route")
		deny(w, http.StatusForbidden, "no credential route "+name+" in this namespace")
		return
	}
	if !c.Allow(r.Method, r.URL.Path) {
		p.record("credential", r.Method, target, false, "not in rules")
		deny(w, http.StatusForbidden, r.Method+" "+r.URL.Path+" is not allowed for "+name)
		return
	}
	value, err := p.secrets.Get(c.Secret)
	if err != nil {
		// The error names the secret, never its value.
		p.record("credential", r.Method, target, false, "secret unavailable")
		deny(w, http.StatusServiceUnavailable, "credential "+name+" is not available")
		return
	}
	up, err := url.Parse(c.Upstream)
	if err != nil {
		deny(w, http.StatusInternalServerError, "bad upstream")
		return
	}
	if p.upstreamFor("https", up.Host) != nil {
		if err := p.checkHost(r.Context(), up.Hostname()); err != nil {
			p.record("credential", r.Method, target, false, err.Error())
			deny(w, http.StatusBadGateway, "upstream not reachable")
			return
		}
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{
		Scheme:   up.Scheme,
		Host:     up.Host,
		Path:     strings.TrimSuffix(up.Path, "/") + r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	out.Host = up.Host
	removeHopHeaders(out.Header)
	format := c.Format
	if format == "" {
		format = "{value}"
	}
	out.Header.Set(c.Header, strings.Replace(format, "{value}", value, 1))
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.record("credential", r.Method, target, false, "upstream error")
		deny(w, http.StatusBadGateway, "upstream error")
		return
	}
	p.record("credential", r.Method, target, true, "")
	copyResponse(w, resp)
}

func splitHostPort(hp string, def int) (string, int) {
	h, ps, err := net.SplitHostPort(hp)
	if err != nil {
		return strings.ToLower(strings.Trim(hp, "[]")), def
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return strings.ToLower(h), -1
	}
	return strings.ToLower(h), port
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush() // keep streaming responses (SSE) streaming
			}
		}
		if err != nil {
			return
		}
	}
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// Serve runs the proxy on l until ctx is done.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
