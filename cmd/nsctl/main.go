// Command nsctl runs agents in namespaces: one sandbox boundary, one network
// policy, one set of credentials and one set of MCP tools per namespace.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/imohiyoko/agent/internal/auth"
	"github.com/imohiyoko/agent/internal/sandbox"
	"github.com/imohiyoko/agent/internal/server"
	"github.com/imohiyoko/agent/internal/spec"
)

const usage = `nsctl: namespaces for agents

Server side (run by a person):
  nsctl validate [-config nsctl.yaml]          check every namespace declaration
  nsctl doctor   [-config nsctl.yaml]          check the machine, files, secrets and commands
  nsctl serve    [-config nsctl.yaml] [-listen addr] [-allow-remote]
  nsctl token create <principal> [-note text] [-ttl 2160h] [-config ...]   -ttl 0: never expires
  nsctl token list   [-config ...]
  nsctl token revoke <id> [-config ...]

Client side (uses NSCTL_URL, default http://127.0.0.1:7070, and NSCTL_TOKEN):
  nsctl whoami
  nsctl namespaces
  nsctl run <namespace> <agent> <prompt>
  nsctl runs <namespace>
  nsctl status <namespace> <run-id>
  nsctl logs <namespace> <run-id> [-f]          -f: follow until the run ends
  nsctl stop <namespace> <run-id>
  nsctl audit <namespace>

Internal:
  nsctl sandbox-init --forward addr=socket ... -- command...
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "validate":
		err = validate(args)
	case "serve":
		err = serve(args)
	case "doctor":
		c, _, cerr := loadConfig(flag.NewFlagSet("doctor", flag.ExitOnError), args)
		if cerr != nil {
			err = cerr
			break
		}
		if n := server.Doctor(c, os.Stdout); n > 0 {
			err = fmt.Errorf("%d problem(s)", n)
		}
	case "token":
		err = token(args)
	case "sandbox-init":
		os.Exit(sandboxInit(args))
	case "whoami", "namespaces", "run", "runs", "status", "logs", "stop", "audit":
		err = client(cmd, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "nsctl: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nsctl:", err)
		os.Exit(1)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) (*server.Config, []string, error) {
	path := fs.String("config", envOr("NSCTL_CONFIG", "nsctl.yaml"), "config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	c, err := server.LoadConfig(*path)
	return c, fs.Args(), err
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func validate(args []string) error {
	c, _, err := loadConfig(flag.NewFlagSet("validate", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	set, err := spec.LoadDir(c.NamespacesDir, c.Home)
	if err != nil {
		return err
	}
	for _, n := range set.Names() {
		ns := set.Namespaces[n]
		fmt.Printf("ok  %-20s agents=%d mcp=%d credential-routes=%d allow=%d\n",
			n, len(ns.Agents), len(ns.MCP.Servers), len(ns.Credentials), len(ns.Network.Allow))
	}
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "API address (overrides the config)")
	remote := fs.Bool("allow-remote", false, "allow an API address that is not loopback")
	c, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *listen != "" {
		c.Listen = *listen
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		if !*remote {
			return fmt.Errorf("listen %s is not loopback; put TLS in front and pass -allow-remote", c.Listen)
		}
		fmt.Fprintln(os.Stderr, "nsctl: WARNING: the API is served without TLS on", c.Listen)
	}
	s, err := server.New(c)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "nsctl: serving the API on", l.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			res, err := s.Reload()
			if err != nil {
				fmt.Fprintln(os.Stderr, "nsctl: reload failed, keeping the current declarations:", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "nsctl: reloaded (added %v, updated %v, removed %v)\n", res.Added, res.Updated, res.Removed)
		}
	}()
	fmt.Fprintf(os.Stderr, "nsctl: pid %d (kill -HUP %d to reload the declarations)\n", os.Getpid(), os.Getpid())
	return s.Run(ctx, l)
}

func token(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("token create|list|revoke")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("token "+sub, flag.ExitOnError)
	note := fs.String("note", "", "what the token is for")
	ttl := fs.Duration("ttl", 90*24*time.Hour, "how long the token is valid (0 = no expiry)")
	// Allow the positional argument before the flags.
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	c, rest, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	pos = append(pos, rest...)
	st := auth.NewStore(c.TokensFile())
	switch sub {
	case "create":
		if len(pos) != 1 {
			return fmt.Errorf("token create <principal>")
		}
		t, value, err := st.CreateTTL(pos[0], *note, *ttl)
		if err != nil {
			return err
		}
		exp := "never expires"
		if t.ExpiresAt != nil {
			exp = "expires " + t.ExpiresAt.Format(time.RFC3339)
		}
		fmt.Fprintf(os.Stderr, "token %s for %s, %s (shown once; store it now)\n", t.ID, t.Principal, exp)
		fmt.Println(value)
	case "list":
		ts, err := st.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPRINCIPAL\tCREATED\tEXPIRES\tNOTE")
		now := time.Now()
		for _, t := range ts {
			exp := "-"
			if t.ExpiresAt != nil {
				exp = t.ExpiresAt.Format("2006-01-02 15:04")
				if t.Expired(now) {
					exp += " (expired)"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Principal, t.CreatedAt.Format("2006-01-02 15:04"), exp, t.Note)
		}
		return tw.Flush()
	case "revoke":
		if len(pos) != 1 {
			return fmt.Errorf("token revoke <id>")
		}
		return st.Revoke(pos[0])
	default:
		return fmt.Errorf("token create|list|revoke")
	}
	return nil
}

func sandboxInit(args []string) int {
	var fwd []string
	for len(args) > 0 {
		switch {
		case args[0] == "--":
			return sandbox.Init(fwd, args[1:])
		case args[0] == "--forward" && len(args) > 1:
			fwd = append(fwd, args[1])
			args = args[2:]
		default:
			fmt.Fprintf(os.Stderr, "nsctl sandbox-init: unexpected %q\n", args[0])
			return 2
		}
	}
	fmt.Fprintln(os.Stderr, "nsctl sandbox-init: missing -- command")
	return 2
}

func client(cmd string, args []string) error {
	base := strings.TrimSuffix(envOr("NSCTL_URL", "http://127.0.0.1:7070"), "/")
	tok := os.Getenv("NSCTL_TOKEN")
	if tok == "" {
		return fmt.Errorf("set NSCTL_TOKEN (create one with: nsctl token create <principal>)")
	}
	follow := false
	if cmd == "logs" && len(args) == 3 && args[2] == "-f" {
		follow, args = true, args[:2]
	}
	need := map[string]int{"whoami": 0, "namespaces": 0, "run": 3, "runs": 1, "status": 2, "logs": 2, "stop": 2, "audit": 1}
	if len(args) != need[cmd] {
		return fmt.Errorf("wrong number of arguments\n\n%s", usage)
	}
	method, path := http.MethodGet, ""
	var body io.Reader
	switch cmd {
	case "whoami":
		path = "/v1/whoami"
	case "namespaces":
		path = "/v1/namespaces"
	case "run":
		method, path = http.MethodPost, "/v1/namespaces/"+args[0]+"/runs"
		b, _ := json.Marshal(map[string]string{"agent": args[1], "prompt": args[2]})
		body = bytes.NewReader(b)
	case "runs":
		path = "/v1/namespaces/" + args[0] + "/runs"
	case "status":
		path = "/v1/namespaces/" + args[0] + "/runs/" + args[1]
	case "logs":
		path = "/v1/namespaces/" + args[0] + "/runs/" + args[1] + "/logs"
		if follow {
			path += "?follow=1"
		}
	case "stop":
		method, path = http.MethodPost, "/v1/namespaces/"+args[0]+"/runs/"+args[1]+"/stop"
	case "audit":
		path = "/v1/namespaces/" + args[0] + "/audit"
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	// The API is local; never send the token through a proxy.
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
