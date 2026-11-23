// Package sandbox wraps a command in the OS sandbox (ADR 0002).
//
//   - Linux: bwrap with its own network namespace. The only ways out are the
//     namespace's unix sockets (proxy, MCP), which `nsctl sandbox-init`
//     exposes inside as 127.0.0.1:3128 and 127.0.0.1:3129.
//   - macOS: sandbox-exec with a Seatbelt profile that only allows outbound
//     connections to the namespace's own loopback ports.
//
// If the sandbox cannot be set up, Command returns an error. It never falls
// back to running the command without a sandbox.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Inner ports used on Linux, inside the network namespace.
const (
	InnerProxyPort = 3128
	InnerMCPPort   = 3129
	innerDir       = "/tmp/.nsctl"
)

// Spec describes one sandboxed command.
type Spec struct {
	Command []string
	Env     []string
	Workdir string

	// Writable paths (besides the temporary directory).
	Writable []string
	// DenyRead paths are hidden. The nsctl state directory must be here.
	DenyRead []string
	// ReadFiles are extra files made readable inside, at the same path on
	// macOS and at InnerPath(dst) on Linux. Keys are names, values host paths.
	ReadFiles map[string]string

	// Linux: unix sockets for the proxy and the MCP gateway ("" = none).
	ProxySocket string
	MCPSocket   string
	// macOS: loopback ports for the proxy and the MCP gateway (0 = none).
	ProxyPort int
	MCPPort   int

	// Self is the nsctl binary (Linux only, for sandbox-init).
	Self string
}

// Endpoints tells the caller what addresses the sandboxed command sees.
type Endpoints struct {
	Proxy string // host:port, "" if none
	MCP   string // host:port, "" if none
	// Files maps ReadFiles names to the path visible inside.
	Files map[string]string
}

// Plan returns the addresses visible inside the sandbox on goos.
func (s Spec) Plan(goos string) Endpoints {
	e := Endpoints{Files: map[string]string{}}
	switch goos {
	case "linux":
		if s.ProxySocket != "" {
			e.Proxy = "127.0.0.1:" + strconv.Itoa(InnerProxyPort)
		}
		if s.MCPSocket != "" {
			e.MCP = "127.0.0.1:" + strconv.Itoa(InnerMCPPort)
		}
		for name := range s.ReadFiles {
			e.Files[name] = filepath.Join(innerDir, "files", name)
		}
	default:
		if s.ProxyPort != 0 {
			e.Proxy = "127.0.0.1:" + strconv.Itoa(s.ProxyPort)
		}
		if s.MCPPort != 0 {
			e.MCP = "127.0.0.1:" + strconv.Itoa(s.MCPPort)
		}
		for name, p := range s.ReadFiles {
			e.Files[name] = p
		}
	}
	return e
}

// Command returns the command wrapped in the sandbox for this OS.
func Command(s Spec) (*exec.Cmd, error) {
	if len(s.Command) == 0 {
		return nil, errors.New("empty command")
	}
	var argv []string
	var err error
	switch runtime.GOOS {
	case "linux":
		argv, err = LinuxArgs(s)
	case "darwin":
		argv, err = DarwinArgs(s)
	default:
		return nil, fmt.Errorf("no sandbox for %s", runtime.GOOS)
	}
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox tool %s not found (it is required; nsctl never runs without a sandbox): %w", argv[0], err)
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env = s.Env
	cmd.Dir = s.Workdir
	return cmd, nil
}

// LinuxArgs builds the bwrap command line.
func LinuxArgs(s Spec) ([]string, error) {
	if s.Self == "" {
		return nil, errors.New("the nsctl binary (Self) is required on Linux")
	}
	a := []string{
		"bwrap",
		"--die-with-parent", "--new-session",
		"--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	for _, w := range s.Writable {
		if _, err := os.Stat(w); err != nil {
			return nil, fmt.Errorf("writable path %s: %w", w, err)
		}
		a = append(a, "--bind", w, w)
	}
	// Hide last, so a hidden path inside a writable one stays hidden.
	for _, d := range s.DenyRead {
		st, err := os.Stat(d)
		if err != nil {
			continue // nothing to hide
		}
		if st.IsDir() {
			a = append(a, "--tmpfs", d)
		} else {
			a = append(a, "--ro-bind", "/dev/null", d)
		}
	}
	a = append(a, "--dir", innerDir, "--ro-bind", s.Self, innerDir+"/nsctl")
	init := []string{innerDir + "/nsctl", "sandbox-init"}
	if s.ProxySocket != "" {
		a = append(a, "--bind", s.ProxySocket, innerDir+"/proxy.sock")
		init = append(init, "--forward", fmt.Sprintf("127.0.0.1:%d=%s/proxy.sock", InnerProxyPort, innerDir))
	}
	if s.MCPSocket != "" {
		a = append(a, "--bind", s.MCPSocket, innerDir+"/mcp.sock")
		init = append(init, "--forward", fmt.Sprintf("127.0.0.1:%d=%s/mcp.sock", InnerMCPPort, innerDir))
	}
	for _, name := range sortedKeys(s.ReadFiles) {
		a = append(a, "--ro-bind", s.ReadFiles[name], filepath.Join(innerDir, "files", name))
	}
	if s.Workdir != "" {
		a = append(a, "--chdir", s.Workdir)
	}
	a = append(a, "--")
	a = append(a, init...)
	a = append(a, "--")
	a = append(a, s.Command...)
	return a, nil
}

// DarwinArgs builds the sandbox-exec command line.
func DarwinArgs(s Spec) ([]string, error) {
	prof, err := DarwinProfile(s)
	if err != nil {
		return nil, err
	}
	return append([]string{"sandbox-exec", "-p", prof}, s.Command...), nil
}

// DarwinProfile builds the Seatbelt profile.
func DarwinProfile(s Spec) (string, error) {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("(version 1)")
	w("(allow default)")
	w(";; network: only this namespace's own loopback ports")
	w("(deny network*)")
	w(`(allow network-bind (local ip "localhost:*"))`)
	w(`(allow network-inbound (local ip "localhost:*"))`)
	for _, p := range []int{s.ProxyPort, s.MCPPort} {
		if p != 0 {
			w(`(allow network-outbound (remote ip "localhost:%d"))`, p)
		}
	}
	w(";; files: write only where declared")
	w("(deny file-write*)")
	w("(allow file-write*")
	for _, p := range append(append([]string{}, s.Writable...), "/private/tmp", "/private/var/folders") {
		q, err := sbString(p)
		if err != nil {
			return "", err
		}
		w("  (subpath %s)", q)
	}
	w(`  (literal "/dev/null") (literal "/dev/zero") (literal "/dev/tty") (literal "/dev/dtracehelper")`)
	w(`  (regex #"^/dev/ttys[0-9]+$") (regex #"^/dev/fd/[0-9]+$"))`)
	if len(s.DenyRead) > 0 {
		w(";; hidden paths (includes the nsctl state directory)")
		for _, p := range s.DenyRead {
			q, err := sbString(p)
			if err != nil {
				return "", err
			}
			w("(deny file-read* file-write* (subpath %s))", q)
		}
	}
	for _, name := range sortedKeys(s.ReadFiles) {
		q, err := sbString(s.ReadFiles[name])
		if err != nil {
			return "", err
		}
		w("(allow file-read* (literal %s))", q)
	}
	return b.String(), nil
}

func sbString(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute", p)
	}
	if strings.ContainsAny(p, "\"\\\n\r\x00") {
		return "", fmt.Errorf("path %q has characters not allowed in a sandbox profile", p)
	}
	// /tmp and /var are symlinks on macOS; the profile needs real paths.
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return `"` + p + `"`, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
