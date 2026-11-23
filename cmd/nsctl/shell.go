package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/imohiyoko/agent/internal/server"
	"github.com/imohiyoko/agent/internal/spec"
)

// shell runs an agent interactively in this terminal, inside its namespace:
//
//	nsctl shell <namespace> [agent] [-config nsctl.yaml]
//
// It needs a running nsctl serve on this machine, started by the same OS
// user with the same config. The agent defaults to the namespace's only
// agent when there is just one.
func shell(args []string) error {
	var pos []string
	for len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		pos, args = append(pos, args[0]), args[1:]
	}
	c, rest, err := loadConfig(flag.NewFlagSet("shell", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	pos = append(pos, rest...)
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("shell <namespace> [agent]")
	}
	ns, agent := pos[0], ""
	if len(pos) == 2 {
		agent = pos[1]
	} else if agent, err = onlyAgent(c, ns); err != nil {
		return err
	}

	// The agent reads the keyboard itself. Keys that would signal nsctl shell
	// (Ctrl-C, Ctrl-\, Ctrl-Z before the agent switches the terminal to raw
	// mode) must not end the session underneath it.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)

	res, err := server.Shell(c, ns, agent, os.Getenv("TERM"), os.Stdin, os.Stdout, os.Stderr, resize)
	if err != nil {
		return err
	}
	if res.State != server.StateExited {
		fmt.Fprintf(os.Stderr, "nsctl: session %s ended: %s\n", res.Run, res.State)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

// onlyAgent returns the namespace's agent when it has exactly one.
func onlyAgent(c *server.Config, ns string) (string, error) {
	set, err := spec.LoadDir(c.NamespacesDir, c.Home)
	if err != nil {
		return "", err
	}
	n := set.Namespaces[ns]
	if n == nil {
		return "", fmt.Errorf("no namespace %s", ns)
	}
	if len(n.Agents) != 1 {
		return "", fmt.Errorf("namespace %s has %d agents; name one: nsctl shell %s <agent>", ns, len(n.Agents), ns)
	}
	return n.Agents[0].Name, nil
}
