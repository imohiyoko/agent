package server

import (
	"crypto/rand"
	"encoding/hex"
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
)

// Run is one agent run, as the API shows it.
type Run struct {
	ID        string     `json:"id"`
	Namespace string     `json:"namespace"`
	Agent     string     `json:"agent"`
	Principal string     `json:"principal"`
	State     string     `json:"state"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	cmd     *exec.Cmd
	logPath string
	stopped bool
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

func (r *runs) start(ns *spec.Namespace, agentName, prompt, principal string) (*Run, error) {
	rt := r.s.runtime(ns.Name)
	a := ns.Agent(agentName)
	if rt == nil || a == nil {
		return nil, ErrNotFound
	}
	r.mu.Lock()
	n := 0
	for _, x := range r.m {
		if x.Namespace == ns.Name && x.State == StateRunning {
			n++
		}
	}
	r.mu.Unlock()
	if n >= r.s.cfg.MaxRunsPerNamespace {
		return nil, ErrLimit
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	run := &Run{
		ID: hex.EncodeToString(idb), Namespace: ns.Name, Agent: a.Name, Principal: principal,
		State: StateRunning, StartedAt: time.Now().UTC(), done: make(chan struct{}),
	}
	dir := filepath.Join(r.s.cfg.PrivateDir(), "runs", run.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	run.logPath = filepath.Join(dir, "output.log")
	out, err := os.OpenFile(run.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd, err := r.s.agentCommand(rt, a, prompt, run.ID)
	if err != nil {
		out.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		out.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	run.cmd = cmd
	r.mu.Lock()
	r.m[run.ID] = run
	r.mu.Unlock()
	r.s.log.Write(audit.Event{Namespace: ns.Name, Kind: "run", Principal: principal, Action: "start", Target: a.Name + "/" + run.ID, Allowed: true})
	go func() {
		err := cmd.Wait()
		out.Close()
		now := time.Now().UTC()
		r.mu.Lock()
		run.EndedAt = &now
		code := cmd.ProcessState.ExitCode()
		run.ExitCode = &code
		switch {
		case run.stopped:
			run.State = StateStopped
		case err == nil:
			run.State = StateExited
		default:
			run.State = StateFailed
			run.Error = err.Error()
		}
		r.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(r.s.cfg.PublicDir(), run.ID))
		close(run.done)
	}()
	return run, nil
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

func (r *runs) stop(ns, id string) error {
	r.mu.Lock()
	x := r.m[id]
	if x == nil || x.Namespace != ns {
		r.mu.Unlock()
		return ErrNotFound
	}
	if x.State != StateRunning {
		r.mu.Unlock()
		return nil
	}
	x.stopped = true
	pid := x.cmd.Process.Pid
	r.mu.Unlock()
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-x.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-x.done
	}
	return nil
}

func (r *runs) stopAll() {
	r.mu.Lock()
	var ids [][2]string
	for _, x := range r.m {
		if x.State == StateRunning {
			ids = append(ids, [2]string{x.Namespace, x.ID})
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.stop(id[0], id[1]) }()
	}
	wg.Wait()
}

func (r *runs) logPath(ns, id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.m[id]
	if x == nil || x.Namespace != ns {
		return ""
	}
	return x.logPath
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
