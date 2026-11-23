package server

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/imohiyoko/agent/internal/auth"
	"github.com/imohiyoko/agent/internal/secrets"
	"github.com/imohiyoko/agent/internal/spec"
)

// Doctor checks the machine and the configuration without changing anything
// and without printing secret values. It returns the number of failures.
func Doctor(c *Config, w io.Writer) int {
	fails := 0
	line := func(level, format string, a ...any) {
		if level == "fail" {
			fails++
		}
		fmt.Fprintf(w, "%-4s  %s\n", level, fmt.Sprintf(format, a...))
	}

	// Sandbox.
	switch runtime.GOOS {
	case "linux":
		if p, err := exec.LookPath("bwrap"); err != nil {
			line("fail", "bwrap is not installed (apt install bubblewrap / dnf install bubblewrap)")
		} else {
			out, err := exec.Command(p, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--unshare-net", "true").CombinedOutput() // sandbox-ok: probing bwrap itself
			if err != nil {
				line("fail", "bwrap cannot create namespaces: %s (on Ubuntu 24.04: sysctl kernel.apparmor_restrict_unprivileged_userns=0, or an AppArmor profile for bwrap)", firstLine(out))
			} else {
				line("ok", "bwrap works (%s)", p)
			}
		}
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			line("fail", "sandbox-exec not found")
		} else if out, err := exec.Command("sandbox-exec", "-p", "(version 1)(allow default)", "true").CombinedOutput(); err != nil { // sandbox-ok: probing sandbox-exec itself
			line("fail", "sandbox-exec does not work: %s", firstLine(out))
		} else {
			line("ok", "sandbox-exec works")
		}
	default:
		line("fail", "%s is not supported (macOS and Linux only)", runtime.GOOS)
	}

	// API address.
	if host, _, err := net.SplitHostPort(c.Listen); err != nil {
		line("fail", "listen %q: %v", c.Listen, err)
	} else if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		line("warn", "listen %s is not loopback; serve needs -allow-remote and TLS in front", c.Listen)
	} else {
		line("ok", "API listens on %s", c.Listen)
	}

	// State and secrets.
	checkMode := func(p string, want os.FileMode, what string) {
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				line("warn", "%s %s does not exist yet", what, p)
				return
			}
			line("fail", "%s %s: %v", what, p, err)
			return
		}
		if st.Mode().Perm()&^want != 0 {
			line("fail", "%s %s has mode %v; want %v or stricter", what, p, st.Mode().Perm(), want)
			return
		}
		line("ok", "%s %s (%v)", what, p, st.Mode().Perm())
	}
	checkMode(c.PrivateDir(), 0o700, "state")
	checkMode(c.TokensFile(), 0o600, "tokens")
	checkMode(c.SecretsFile, 0o600, "secrets")

	if ts, err := auth.NewStore(c.TokensFile()).List(); err != nil {
		line("fail", "tokens: %v", err)
	} else {
		expired, soon := 0, 0
		now := time.Now()
		for _, t := range ts {
			switch {
			case t.Expired(now):
				expired++
			case t.ExpiresAt != nil && t.ExpiresAt.Before(now.Add(14*24*time.Hour)):
				soon++
			}
		}
		line("ok", "%d token(s)", len(ts))
		if expired > 0 {
			line("warn", "%d expired token(s); remove them with nsctl token revoke", expired)
		}
		if soon > 0 {
			line("warn", "%d token(s) expire within 14 days", soon)
		}
	}

	// Declarations.
	set, err := spec.LoadDir(c.NamespacesDir, c.Home)
	if err != nil {
		line("fail", "namespaces: %v", err)
		return fails
	}
	line("ok", "%d namespace(s) in %s", len(set.Namespaces), c.NamespacesDir)
	src := secrets.File{Path: c.SecretsFile}
	for _, name := range set.Names() {
		ns := set.Namespaces[name]
		var names []string
		for _, cr := range ns.Credentials {
			names = append(names, cr.Secret)
		}
		for _, m := range ns.MCP.Servers {
			for _, v := range m.Env {
				if v.Secret != "" {
					names = append(names, v.Secret)
				}
			}
		}
		sort.Strings(names)
		for _, sn := range names {
			if _, err := src.Get(sn); err != nil {
				line("fail", "%s: secret %s: %v", name, sn, err)
			}
		}
		for _, p := range ns.Filesystem.Writable {
			if _, err := os.Stat(p); err != nil {
				line("fail", "%s: writable path %s does not exist", name, p)
			}
		}
		for _, a := range ns.Agents {
			if _, err := exec.LookPath(a.Command[0]); err != nil {
				line("fail", "%s: agent %s: command %s not found", name, a.Name, a.Command[0])
			}
			for _, msg := range claudeAuthProblems(ns, &a) {
				line("warn", "%s: agent %s: %s", name, a.Name, msg)
			}
		}
		for _, m := range ns.MCP.Servers {
			if len(m.Command) == 0 {
				continue // remote server
			}
			if _, err := exec.LookPath(m.Command[0]); err != nil {
				line("fail", "%s: mcp server %s: command %s not found", name, m.Name, m.Command[0])
			}
		}
		for _, d := range ns.Filesystem.Writable {
			if isInside(d, c.StateDir) || isInside(c.StateDir, d) {
				line("fail", "%s: writable path %s overlaps the state directory", name, d)
			}
		}
	}
	if fails == 0 {
		line("ok", "no problems found")
	}
	return fails
}

func isInside(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}

// claudeAuthProblems finds Claude Code settings that would not work in a
// namespace: both an API key and a subscription (OAuth) token, or a base URL
// that is not one of the namespace's credential routes.
func claudeAuthProblems(ns *spec.Namespace, a *spec.Agent) []string {
	var out []string
	_, apiKey := a.Env["ANTHROPIC_API_KEY"]
	_, oauth := a.Env["CLAUDE_CODE_OAUTH_TOKEN"]
	if apiKey && oauth {
		out = append(out, "both ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN are set; Claude Code uses the API key, so set only one")
	}
	base, ok := a.Env["ANTHROPIC_BASE_URL"]
	if !ok {
		if apiKey || oauth {
			out = append(out, "ANTHROPIC_BASE_URL is not set; set it to http://<route>"+spec.CredsSuffix+" so the proxy adds the real key")
		}
		return out
	}
	host, isHTTP := strings.CutPrefix(base, "http://")
	route, isRoute := strings.CutSuffix(strings.TrimSuffix(host, "/"), spec.CredsSuffix)
	if !isHTTP || !isRoute || ns.Credential(route) == nil {
		out = append(out, fmt.Sprintf("ANTHROPIC_BASE_URL %q is not a credential route of this namespace (http://<route>%s)", base, spec.CredsSuffix))
	}
	return out
}
