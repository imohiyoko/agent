package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDarwinProfile(t *testing.T) {
	p, err := DarwinProfile(Spec{
		Writable:  []string{"/Users/u/work"},
		DenyRead:  []string{"/Users/u/.ssh", "/Users/u/.local/state/nsctl/private"},
		ProxyPort: 40001, MCPPort: 40002,
		ReadFiles: map[string]string{"mcp.json": "/Users/u/.local/state/nsctl/public/r/mcp.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny network*)",
		`(allow network-outbound (remote ip "localhost:40001"))`,
		`(allow network-outbound (remote ip "localhost:40002"))`,
		"(deny file-write*)",
		`(subpath "/Users/u/work")`,
		`(deny file-read* file-write* (subpath "/Users/u/.ssh"))`,
		`(deny file-read* file-write* (subpath "/Users/u/.local/state/nsctl/private"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %s\n%s", want, p)
		}
	}
	if strings.Contains(p, `remote ip "*`) || strings.Contains(p, "(allow network-outbound)") {
		t.Errorf("profile allows any outbound:\n%s", p)
	}
}

func TestDarwinProfileRejectsQuotes(t *testing.T) {
	if _, err := DarwinProfile(Spec{Writable: []string{`/tmp/a") (allow default`}}); err == nil {
		t.Fatal("a path that breaks out of the profile string was accepted")
	}
	if _, err := DarwinProfile(Spec{Writable: []string{"relative"}}); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestLinuxArgs(t *testing.T) {
	dir := t.TempDir()
	a, err := LinuxArgs(Spec{
		Command: []string{"echo", "hi"}, Writable: []string{dir}, DenyRead: []string{dir + "/missing"},
		ProxySocket: "/s/proxy.sock", Self: "/bin/nsctl",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(a, " ")
	for _, want := range []string{"--unshare-net", "--die-with-parent", "--bind " + dir + " " + dir, "--bind /s/proxy.sock /tmp/.nsctl/proxy.sock", "-- echo hi"} {
		if !strings.Contains(s, want) {
			t.Errorf("args lack %q: %s", want, s)
		}
	}
	if _, err := LinuxArgs(Spec{Command: []string{"x"}, Writable: []string{"/does/not/exist"}, Self: "/bin/nsctl"}); err == nil {
		t.Error("missing writable path accepted")
	}
}

// TestLinuxSandbox runs a real bwrap sandbox. It is skipped where bwrap is
// not available.
func TestLinuxSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	work := t.TempDir()
	// Outside /tmp (which is replaced inside the sandbox), so hiding is tested.
	home, _ := os.UserHomeDir()
	secret, err := os.MkdirTemp(home, ".nsctl-test-secret-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(secret)
	os.WriteFile(filepath.Join(secret, "key"), []byte("hidden"), 0o600)
	self, _ := os.Executable()
	sp := Spec{
		Command:  []string{"sh", "-c", `echo ok > ` + work + `/out; cat ` + secret + `/key 2>/dev/null && echo LEAK; echo x > /etc/nsctl-test 2>/dev/null && echo WROTE; getent hosts example.com >/dev/null 2>&1 && echo NET; echo done`},
		Env:      []string{"PATH=" + os.Getenv("PATH")},
		Writable: []string{work}, DenyRead: []string{secret}, Self: self, Workdir: work,
	}
	cmd, err := Command(sp)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No permissions to create new namespace") || strings.Contains(string(out), "Operation not permitted") {
			t.Skip("user namespaces not available: " + string(out))
		}
		t.Fatalf("%v: %s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "done") {
		t.Fatalf("command did not run: %s", s)
	}
	for _, bad := range []string{"LEAK", "WROTE", "NET"} {
		if strings.Contains(s, bad) {
			t.Errorf("sandbox let %s through: %s", bad, s)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(work, "out")); string(b) != "ok\n" {
		t.Errorf("write to the workdir failed: %q", b)
	}
}

// The test binary doubles as nsctl for sandbox-init.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-init" {
		args := os.Args[2:]
		var fwd []string
		for len(args) > 0 && args[0] != "--" {
			fwd = append(fwd, args[1])
			args = args[2:]
		}
		os.Exit(Init(fwd, args[1:]))
	}
	os.Exit(m.Run())
}

// TestDarwinSandbox runs a real sandbox-exec sandbox (macOS CI runner).
func TestDarwinSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	work := t.TempDir()
	home, _ := os.UserHomeDir()
	secret, err := os.MkdirTemp(home, ".nsctl-test-secret-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(secret)
	os.WriteFile(filepath.Join(secret, "key"), []byte("hidden"), 0o600)
	outside, err := os.MkdirTemp(home, ".nsctl-test-outside-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside)
	sp := Spec{
		Command:  []string{"sh", "-c", `echo ok > ` + work + `/out; cat ` + secret + `/key 2>/dev/null && echo LEAK; echo x > ` + outside + `/f 2>/dev/null && echo WROTE; curl -s --max-time 5 https://example.com >/dev/null 2>&1 && echo NET; echo done`},
		Env:      []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home},
		Writable: []string{work}, DenyRead: []string{secret}, Workdir: work, ProxyPort: 1,
	}
	cmd, err := Command(sp)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "done") {
		t.Fatalf("command did not run: %s", s)
	}
	for _, bad := range []string{"LEAK", "WROTE", "NET"} {
		if strings.Contains(s, bad) {
			t.Errorf("sandbox let %s through: %s", bad, s)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(work, "out")); string(b) != "ok\n" {
		t.Errorf("write to the workdir failed: %q", b)
	}
}
