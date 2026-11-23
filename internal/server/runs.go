package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/imohiyoko/agent/internal/audit"
	"github.com/imohiyoko/agent/internal/spec"
)

// Run states.
const (
	StateRunning = "running"
	StateExited  = "exited"
	StateFailed  = "failed"
	StateStopped = "stopped"
	StateTimeout = "timeout"
	// StateLost is a run that was running when nsctl stopped. Its process
	// died with nsctl (bwrap --die-with-parent; the process group on macOS).
	StateLost = "lost"
)

// Run modes.
const (
	ModePrompt      = "prompt"      // nsctl run / the API: one prompt, output logged
	ModeInteractive = "interactive" // nsctl shell: a person's terminal, not logged
)

// Run is one agent run, as the API shows it.
type Run struct {
	ID        string     `json:"id"`
	Namespace string     `json:"namespace"`
	Agent     string     `json:"agent"`
	Mode      string     `json:"mode"` // ModePrompt or ModeInteractive
	Principal string     `json:"principal"`
	State     string     `json:"state"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Deadline  *time.Time `json:"deadline,omitempty"`

	cmd     *exec.Cmd
	logPath string
	why     string // "stop" or "timeout" when nsctl ended it
	done    chan struct{}
}

type runs struct {
	s  *Server
	mu sync.Mutex
	m  map[string]*Run
}

func newRuns(s *Server) *runs { return &runs{s: s, m: map[string]*Run{}} }

// ErrLimit is returned when a namespace has too many runs.
var ErrLimit = errors.New("too many runs in this namespace")

func (r *runs) dir(id string) string { return filepath.Join(r.s.cfg.PrivateDir(), "runs", id) }

// save writes the run's record. Called with r.mu held.
func (r *runs) save(x *Run) {
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(r.dir(x.ID), "run.json")
	_ = os.WriteFile(p+".tmp", b, 0o600)
	_ = os.Rename(p+".tmp", p)
}

// load reads the records of earlier runs. Runs that were running when nsctl
// stopped are marked lost.
func (r *runs) load() error {
	root := filepath.Join(r.s.cfg.PrivateDir(), "runs")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "run.json"))
		if err != nil {
			continue
		}
		var x Run
		if json.Unmarshal(b, &x) != nil || x.ID != e.Name() {
			continue
		}
		if x.Mode == "" { // saved before modes existed
			x.Mode = ModePrompt
		}
		x.logPath = filepath.Join(root, x.ID, "output.log")
		x.done = make(chan struct{})
		close(x.done)
		if x.State == StateRunning {
			x.State = StateLost
			r.save(&x)
		}
		r.m[x.ID] = &x
	}
	return nil
}

func (r *runs) timeout(a *spec.Agent) time.Duration {
	if a.TimeoutDur > 0 {
		return a.TimeoutDur
	}
	if r.s.cfg.runTimeout > 0 {
		return r.s.cfg.runTimeout
	}
	d, err := time.ParseDuration(r.s.cfg.DefaultRunTimeout)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// ErrNoInteractive is returned by nsctl shell for an agent without an
// interactive command.
var ErrNoInteractive = errors.New("this agent has no interactive command (set agents[].interactive)")

// start starts an agent. Prompt runs log their output and stop at the
// timeout. Interactive runs use the person's terminal, are not logged and
// have no timeout: they end when the agent exits, the terminal goes away or
// someone stops them.
func (r *runs) start(ns *spec.Namespace, agentName, principal string, l launch) (*Run, error) {
	rt := r.s.runtime(ns.Name)
	a := ns.Agent(agentName)
	if rt == nil || a == nil {
		return nil, ErrNotFound
	}
	interactive := l.Terminal != nil
	if interactive && len(a.Interactive) == 0 {
		return nil, ErrNoInteractive
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	now := time.Now().UTC()
	run := &Run{
		ID: hex.EncodeToString(idb), Namespace: ns.Name, Agent: a.Name, Principal: principal,
		Mode: ModePrompt, State: StateRunning, StartedAt: now, done: make(chan struct{}),
	}
	limit := r.timeout(a)
	if interactive {
		run.Mode = ModeInteractive
	} else {
		deadline := now.Add(limit)
		run.Deadline = &deadline
	}

	// Count and reserve under one lock so two requests cannot both take the
	// last slot.
	r.mu.Lock()
	n := 0
	for _, x := range r.m {
		if x.Namespace == ns.Name && x.State == StateRunning {
			n++
		}
	}
	if n >= r.s.cfg.MaxRunsPerNamespace {
		r.mu.Unlock()
		return nil, ErrLimit
	}
	r.m[run.ID] = run
	r.mu.Unlock()
	fail := func(err error) (*Run, error) {
		r.mu.Lock()
		delete(r.m, run.ID)
		r.mu.Unlock()
		_ = os.RemoveAll(r.dir(run.ID))
		return nil, err
	}

	if err := os.MkdirAll(r.dir(run.ID), 0o700); err != nil {
		return fail(err)
	}
	run.logPath = filepath.Join(r.dir(run.ID), "output.log")
	f, err := os.OpenFile(run.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	cmd, err := r.s.agentCommand(rt, ns, a, l, run.ID)
	if err != nil {
		f.Close()
		return fail(err)
	}
	if interactive {
		// The output goes to the person's terminal only.
		fmt.Fprintf(f, "nsctl: interactive session in a terminal; its output is not recorded\n")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = l.Terminal.Stdin, l.Terminal.Stdout, l.Terminal.Stderr
		// A session of its own, so the terminal's job control and the nsctl
		// process's own terminal (if any) do not get mixed up.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		out := &capped{f: f, left: r.s.cfg.MaxLogBytes}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return fail(fmt.Errorf("start: %w", err))
	}
	r.mu.Lock()
	run.cmd = cmd
	r.save(run)
	r.mu.Unlock()
	r.s.log.Write(audit.Event{Namespace: ns.Name, Kind: "run", Principal: principal, Action: "start", Target: a.Name + "/" + run.ID, Allowed: true, Reason: run.Mode})

	var timer *time.Timer // nil for interactive runs: no timeout
	if !interactive {
		timer = time.AfterFunc(limit, func() { r.end(run, "timeout") })
	}
	go func() {
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		f.Close()
		now := time.Now().UTC()
		r.mu.Lock()
		run.EndedAt = &now
		code := cmd.ProcessState.ExitCode()
		run.ExitCode = &code
		switch {
		case run.why == "timeout":
			run.State = StateTimeout
		case run.why == "stop":
			run.State = StateStopped
		case err == nil:
			run.State = StateExited
		default:
			run.State = StateFailed
			run.Error = err.Error()
		}
		r.save(run)
		r.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(r.s.cfg.PublicDir(), run.ID))
		r.s.log.Write(audit.Event{Namespace: run.Namespace, Kind: "run", Principal: run.Principal, Action: "end", Target: run.Agent + "/" + run.ID, Allowed: true, Reason: run.State})
		close(run.done)
	}()
	return run, nil
}

// end stops a running run: SIGTERM to its process group, SIGKILL after 10s.
func (r *runs) end(x *Run, why string) {
	r.mu.Lock()
	if x.State != StateRunning || x.cmd == nil || x.why != "" {
		r.mu.Unlock()
		return
	}
	x.why = why
	pid := x.cmd.Process.Pid
	r.mu.Unlock()
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-x.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-x.done
	}
}

func (r *runs) get(ns, id string) *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.m[id]
	if x == nil || x.Namespace != ns {
		return nil
	}
	cp := *x
	return &cp
}

func (r *runs) list(ns string) []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Run{}
	for _, x := range r.m {
		if x.Namespace == ns {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

func (r *runs) find(ns, id string) *Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.m[id]
	if x == nil || x.Namespace != ns {
		return nil
	}
	return x
}

func (r *runs) stop(ns, id string) error {
	x := r.find(ns, id)
	if x == nil {
		return ErrNotFound
	}
	r.end(x, "stop")
	return nil
}

func (r *runs) running(filter func(*Run) bool) []*Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Run
	for _, x := range r.m {
		if x.State == StateRunning && filter(x) {
			out = append(out, x)
		}
	}
	return out
}

func (r *runs) stopEach(xs []*Run) {
	var wg sync.WaitGroup
	for _, x := range xs {
		wg.Add(1)
		go func() { defer wg.Done(); r.end(x, "stop") }()
	}
	wg.Wait()
}

func (r *runs) stopAll() { r.stopEach(r.running(func(*Run) bool { return true })) }

func (r *runs) stopNamespace(ns string) {
	r.stopEach(r.running(func(x *Run) bool { return x.Namespace == ns }))
}

func (r *runs) logPath(ns, id string) string {
	x := r.find(ns, id)
	if x == nil {
		return ""
	}
	return x.logPath
}

// doneCh returns a channel closed when the run ends.
func (r *runs) doneCh(ns, id string) <-chan struct{} {
	x := r.find(ns, id)
	if x == nil {
		return nil
	}
	return x.done
}

// wait blocks until the run ends (tests).
func (r *runs) wait(id string) {
	r.mu.Lock()
	x := r.m[id]
	r.mu.Unlock()
	if x != nil {
		<-x.done
	}
}

// capped writes up to left bytes, then one notice, then drops the rest.
type capped struct {
	mu   sync.Mutex
	f    *os.File
	left int64
	cut  bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cut {
		return len(p), nil
	}
	w := p
	if int64(len(w)) > c.left {
		w = w[:c.left]
	}
	n, err := c.f.Write(w)
	c.left -= int64(n)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		c.cut = true
		_, _ = c.f.WriteString("\n[nsctl: output truncated at max_log_bytes]\n")
	}
	return len(p), nil
}
