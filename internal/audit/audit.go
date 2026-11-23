// Package audit writes one JSON line per decision.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one decision. Never put secret values in it.
type Event struct {
	Time      time.Time `json:"time"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind"` // "egress", "credential", "mcp", "api", "run"
	Principal string    `json:"principal,omitempty"`
	Action    string    `json:"action,omitempty"`
	Target    string    `json:"target,omitempty"`
	Allowed   bool      `json:"allowed"`
	Reason    string    `json:"reason,omitempty"`
}

// Log appends events to <dir>/<namespace>.jsonl (or _api.jsonl when there is
// no namespace).
type Log struct {
	dir string
	mu  sync.Mutex
}

// New returns a log writing under dir.
func New(dir string) *Log { return &Log{dir: dir} }

// Write appends e. Errors are ignored on purpose: a full disk must not turn
// a deny into an allow, and callers decide before they log.
func (l *Log) Write(e Event) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	name := e.Namespace
	if name == "" {
		name = "_api"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(l.dir, 0o700)
	f, err := os.OpenFile(filepath.Join(l.dir, name+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Read returns up to the last n events for a namespace.
func (l *Log) Read(namespace string, n int) ([]Event, error) {
	f, err := os.Open(filepath.Join(l.dir, namespace+".jsonl"))
	if os.IsNotExist(err) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}
