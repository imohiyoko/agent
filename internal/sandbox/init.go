package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// Init runs inside the Linux sandbox. It listens on each "addr=socket"
// forward, copies connections to the unix socket, runs the command and exits
// with its status.
func Init(forwards []string, command []string) int {
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "nsctl sandbox-init: no command")
		return 2
	}
	for _, f := range forwards {
		addr, sock, ok := strings.Cut(f, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "nsctl sandbox-init: bad forward %q\n", f)
			return 2
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nsctl sandbox-init: listen %s: %v\n", addr, err)
			return 2
		}
		go forward(l, sock)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "nsctl sandbox-init: %v\n", err)
		return 127
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

func forward(l net.Listener, sock string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			u, err := net.Dial("unix", sock)
			if err != nil {
				c.Close()
				return
			}
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); _ = u.(*net.UnixConn).CloseWrite(); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); _ = c.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
			<-done
			<-done
			c.Close()
			u.Close()
		}()
	}
}
