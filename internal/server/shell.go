package server

// Interactive sessions (nsctl shell, ADR 0005).
//
// nsctl shell runs on the same machine as nsctl serve, as the same OS user.
// It connects to the control socket in the private state directory and hands
// over its terminal: the three file descriptors travel in the first message
// (SCM_RIGHTS). The server starts the agent's interactive command in the
// namespace's sandbox with those descriptors, so the agent talks to the
// person's terminal directly and nothing is copied through nsctl.
//
// Messages are JSON lines.
//
//	client → server  shellRequest (with the three descriptors)
//	server → client  shellReply{Run: id}  or  shellReply{Error: ...}
//	client → server  shellControl{Resize: true}   when the window changes
//	server → client  shellReply{State, ExitCode}  when the agent ends
//
// If the client goes away (the terminal or the pane is closed), the session
// is stopped.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"syscall"

	"github.com/imohiyoko/agent/internal/audit"
)

// LocalPrincipal is the principal recorded for nsctl shell. Only the OS user
// that runs nsctl can reach the control socket, and that user can already
// read every token and secret, so it is treated like an admin.
const LocalPrincipal = "local"

// ControlSocket is where nsctl shell connects. It is inside the private
// state directory (mode 0700), which no sandbox can see.
func (c *Config) ControlSocket() string { return c.PrivateDir() + "/control.sock" }

type shellRequest struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Term      string `json:"term"`
}

type shellControl struct {
	Resize bool `json:"resize,omitempty"`
}

type shellReply struct {
	Run      string `json:"run,omitempty"`
	Error    string `json:"error,omitempty"`
	State    string `json:"state,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// termRe limits TERM to names terminfo can have; anything else is replaced.
var termRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// serveControl accepts nsctl shell connections until ctx is done.
func (s *Server) serveControl(ctx context.Context) error {
	path := s.cfg.ControlSocket()
	_ = os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveShell(c)
	}
}

func (s *Server) serveShell(c *net.UnixConn) {
	defer c.Close()
	reply := func(r shellReply) {
		b, _ := json.Marshal(r)
		_, _ = c.Write(append(b, '\n'))
	}

	req, files, err := readShellRequest(c)
	defer func() {
		for _, f := range files {
			f.Close() // the agent has its own copies once started
		}
	}()
	if err != nil {
		reply(shellReply{Error: err.Error()})
		return
	}
	ns := s.nsSet().Namespaces[req.Namespace]
	if ns == nil {
		reply(shellReply{Error: "no namespace " + req.Namespace})
		return
	}
	term := req.Term
	if !termRe.MatchString(term) {
		term = "xterm-256color"
	}
	run, err := s.runs.start(ns, req.Agent, LocalPrincipal, launch{Terminal: &terminal{
		Stdin: files[0], Stdout: files[1], Stderr: files[2], Term: term,
	}})
	if err != nil {
		s.log.Write(audit.Event{Namespace: req.Namespace, Kind: "run", Principal: LocalPrincipal, Action: "shell", Target: req.Agent, Allowed: false, Reason: err.Error()})
		if errors.Is(err, ErrNotFound) {
			err = fmt.Errorf("no agent %s in namespace %s", req.Agent, req.Namespace)
		}
		reply(shellReply{Error: err.Error()})
		return
	}
	reply(shellReply{Run: run.ID})

	// Control messages until the client goes away.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			var m shellControl
			if json.Unmarshal(sc.Bytes(), &m) == nil && m.Resize {
				s.runs.resize(run)
			}
		}
	}()
	select {
	case <-run.done:
	case <-gone:
		s.runs.end(run, "stop")
	}
	s.runs.mu.Lock()
	r := shellReply{State: run.State, ExitCode: run.ExitCode}
	s.runs.mu.Unlock()
	reply(r)
}

// readShellRequest reads the first message and the three descriptors sent
// with it.
func readShellRequest(c *net.UnixConn) (shellRequest, []*os.File, error) {
	var req shellRequest
	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(3*4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return req, nil, err
	}
	var files []*os.File
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err == nil {
		for _, m := range msgs {
			fds, err := syscall.ParseUnixRights(&m)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "terminal"))
			}
		}
	}
	if len(files) != 3 {
		return req, files, fmt.Errorf("expected 3 descriptors (stdin, stdout, stderr), got %d", len(files))
	}
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return req, files, errors.New("bad request")
	}
	return req, files, nil
}

// resize tells an interactive agent that the window size changed.
//
// On macOS the process nsctl started is the agent itself (sandbox-exec
// replaces itself with it). On Linux it is bwrap, which does not pass signals
// on; there, nsctl sandbox-init watches the terminal size and signals the
// agent itself, so nothing is needed here.
func (r *runs) resize(x *Run) {
	if r.s.goos != "darwin" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if x.State == StateRunning && x.cmd != nil {
		_ = x.cmd.Process.Signal(syscall.SIGWINCH)
	}
}

// ShellResult is how an interactive session ended.
type ShellResult struct {
	Run      string
	State    string
	ExitCode int
}

// Shell runs an agent interactively on the given terminal through a running
// nsctl serve. resize delivers a value whenever the window size changes.
func Shell(c *Config, namespace, agent, term string, stdin, stdout, stderr *os.File, resize <-chan os.Signal) (ShellResult, error) {
	var res ShellResult
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: c.ControlSocket(), Net: "unix"})
	if err != nil {
		return res, fmt.Errorf("cannot reach nsctl serve (is it running with this config?): %w", err)
	}
	defer conn.Close()
	b, _ := json.Marshal(shellRequest{Namespace: namespace, Agent: agent, Term: term})
	rights := syscall.UnixRights(int(stdin.Fd()), int(stdout.Fd()), int(stderr.Fd()))
	if _, _, err := conn.WriteMsgUnix(b, rights, nil); err != nil {
		return res, err
	}

	replies := json.NewDecoder(conn)
	var first shellReply
	if err := replies.Decode(&first); err != nil {
		return res, fmt.Errorf("nsctl serve: %w", err)
	}
	if first.Error != "" {
		return res, errors.New(first.Error)
	}
	res.Run = first.Run

	go func() {
		for range resize {
			if _, err := conn.Write([]byte(`{"resize":true}` + "\n")); err != nil {
				return
			}
		}
	}()
	var last shellReply
	if err := replies.Decode(&last); err != nil {
		if errors.Is(err, io.EOF) {
			return res, errors.New("nsctl serve closed the session")
		}
		return res, err
	}
	res.State = last.State
	if last.ExitCode != nil {
		res.ExitCode = *last.ExitCode
	}
	return res, nil
}
