package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/imohiyoko/agent/internal/secrets"
)

// secretNameRe is "<namespace>/<name>", the form declarations refer to.
var secretNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}/[A-Za-z0-9._-]{1,128}$`)

// secret manages the secrets file without printing values:
//
//	nsctl secret set <namespace>/<name>   value from stdin (hidden on a terminal)
//	nsctl secret list                     names only
//	nsctl secret rm <namespace>/<name>
func secret(args []string) error {
	if len(args) == 0 {
		return errors.New("secret set|list|rm")
	}
	sub, args := args[0], args[1:]
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	c, rest, err := loadConfig(flag.NewFlagSet("secret "+sub, flag.ExitOnError), args)
	if err != nil {
		return err
	}
	pos = append(pos, rest...)
	f := secrets.File{Path: c.SecretsFile}

	switch sub {
	case "list":
		names, err := f.Names()
		if err != nil {
			return err
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	case "set", "rm":
		if len(pos) != 1 || !secretNameRe.MatchString(pos[0]) {
			return fmt.Errorf("secret %s <namespace>/<name>", sub)
		}
		if sub == "rm" {
			return f.Delete(pos[0])
		}
		v, err := readSecret(os.Stdin, os.Stderr, pos[0])
		if err != nil {
			return err
		}
		if err := f.Set(pos[0], v); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "set %s in %s (credential routes use it from the next request; MCP servers from their next start)\n", pos[0], c.SecretsFile)
		return nil
	}
	return errors.New("secret set|list|rm")
}

// readSecret reads one line. On a terminal it asks for the value and turns
// echo off, so the value is neither shown nor kept in the shell history.
func readSecret(in *os.File, prompt io.Writer, name string) (string, error) {
	if restore, err := noEcho(in); err == nil {
		fmt.Fprintf(prompt, "value for %s (not shown): ", name)
		defer func() {
			restore()
			fmt.Fprintln(prompt)
		}()
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no value on stdin")
	}
	v := strings.TrimRight(line, "\r\n")
	if v == "" {
		return "", errors.New("empty value")
	}
	return v, nil
}
