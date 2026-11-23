package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const home = "/home/u"

func valid() *Namespace {
	return &Namespace{
		Version: 1,
		Name:    "research",
		Members: []Member{{Principal: "alice", Role: RoleOperator}},
		Network: Network{Allow: []HostRule{{Host: "api.github.com", Ports: []int{443}}, {Host: "*.pypi.org", Ports: []int{443}}}},
		Credentials: []Credential{{
			Name: "github", Upstream: "https://api.github.com", Header: "Authorization",
			Format: "Bearer {value}", Secret: "research/github",
			Rules: []HTTPRule{{Methods: []string{"get"}, Paths: []string{"/repos/**"}}},
		}},
		Filesystem: Filesystem{Writable: []string{"~/work/research"}, DenyRead: []string{"~/.ssh"}},
		MCP: MCP{Servers: []MCPServer{{
			Name: "files", Command: []string{"server"}, Tools: []string{"read_file"},
			Env: map[string]EnvVal{"TOKEN": {Secret: "research/x"}},
		}}},
		Agents: []Agent{{Name: "claude", Command: []string{"claude", "-p", "{prompt}"}, Workdir: "~/work/research"}},
	}
}

func TestValidNamespace(t *testing.T) {
	ns := valid()
	if err := ns.Validate(home); err != nil {
		t.Fatal(err)
	}
	if ns.Agents[0].Workdir != "/home/u/work/research" {
		t.Errorf("workdir not expanded: %s", ns.Agents[0].Workdir)
	}
	if ns.Credentials[0].Rules[0].Methods[0] != "GET" {
		t.Errorf("method not upper-cased")
	}
}

// Each case breaks one invariant from docs/design.md and must be rejected.
func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		edit func(*Namespace)
		want string
	}{
		"bare wildcard host":       {func(n *Namespace) { n.Network.Allow[0].Host = "*" }, "bare *"},
		"wildcard in the middle":   {func(n *Namespace) { n.Network.Allow[0].Host = "a.*.com" }, "bare *"},
		"no ports":                 {func(n *Namespace) { n.Network.Allow[0].Ports = nil }, "ports must be listed"},
		"reserved creds host":      {func(n *Namespace) { n.Network.Allow[0].Host = "x.creds.internal" }, "reserved"},
		"other namespace secret":   {func(n *Namespace) { n.Credentials[0].Secret = "other/github" }, "must belong to this namespace"},
		"mcp other ns secret":      {func(n *Namespace) { n.MCP.Servers[0].Env["TOKEN"] = EnvVal{Secret: "other/x"} }, "must belong to this namespace"},
		"plain http upstream":      {func(n *Namespace) { n.Credentials[0].Upstream = "http://x" }, "https://"},
		"credential without rules": {func(n *Namespace) { n.Credentials[0].Rules = nil }, "rules must be listed"},
		"method wildcard":          {func(n *Namespace) { n.Credentials[0].Rules[0].Methods = []string{"*"} }, "method *"},
		"mcp tool wildcard":        {func(n *Namespace) { n.MCP.Servers[0].Tools = []string{"*"} }, "no wildcards"},
		"mcp no tools":             {func(n *Namespace) { n.MCP.Servers[0].Tools = nil }, "tools must be listed"},
		"writable root":            {func(n *Namespace) { n.Filesystem.Writable = []string{"/"} }, "too broad"},
		"writable home":            {func(n *Namespace) { n.Filesystem.Writable = []string{"~"} }, "too broad"},
		"workdir outside":          {func(n *Namespace) { n.Agents[0].Workdir = "/etc" }, "inside filesystem.writable"},
		"prompt inside argument":   {func(n *Namespace) { n.Agents[0].Command = []string{"sh", "-c", "echo {prompt}"} }, "whole argument"},
		"bad role":                 {func(n *Namespace) { n.Members[0].Role = "owner" }, "role"},
		"duplicate member":         {func(n *Namespace) { n.Members = append(n.Members, n.Members[0]) }, "twice"},
		"bad version":              {func(n *Namespace) { n.Version = 2 }, "version"},
		"bad name":                 {func(n *Namespace) { n.Name = "Research" }, "name"},
		"mcp neither":              {func(n *Namespace) { n.MCP.Servers[0].Command = nil }, "set command"},
		"mcp both":                 {func(n *Namespace) { n.MCP.Servers[0].Credential = "github" }, "not both"},
		"mcp unknown route": {func(n *Namespace) {
			n.MCP.Servers[0].Command, n.MCP.Servers[0].Env, n.MCP.Servers[0].Credential = nil, nil, "nope"
		}, "not a credential route"},
		"bad timeout":      {func(n *Namespace) { n.Agents[0].Timeout = "soon" }, "timeout"},
		"negative timeout": {func(n *Namespace) { n.Agents[0].Timeout = "-1m" }, "timeout"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ns := valid()
			c.edit(ns)
			err := ns.Validate(home)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadDirRejectsUnknownFieldsAndNameMismatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", "version: 1\nname: a\nnetwork:\n  alow: []\n")
	if _, err := LoadDir(dir, home); err == nil || !strings.Contains(err.Error(), "alow") {
		t.Fatalf("typo must be an error, got %v", err)
	}
	os.Remove(filepath.Join(dir, "a.yaml"))
	write("a.yaml", "version: 1\nname: b\n")
	if _, err := LoadDir(dir, home); err == nil || !strings.Contains(err.Error(), "match the file name") {
		t.Fatalf("name mismatch must be an error, got %v", err)
	}
}

func TestExampleNamespaces(t *testing.T) {
	set, err := LoadDir("../../examples/namespaces", home)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Namespaces) == 0 {
		t.Fatal("no examples")
	}
}

func TestHostRule(t *testing.T) {
	n := Network{
		Allow: []HostRule{{Host: "*.example.com", Ports: []int{443}}, {Host: "api.github.com", Ports: []int{443}}},
		Deny:  []HostRule{{Host: "evil.example.com", Ports: []int{443}}},
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"a.example.com", 443, true},
		{"a.b.example.com", 443, true},
		{"A.Example.COM.", 443, true},
		{"example.com", 443, false},
		{"badexample.com", 443, false},
		{"a.example.com", 80, false},
		{"evil.example.com", 443, false},
		{"api.github.com", 443, true},
		{"api.github.com.evil.io", 443, false},
		{"other.com", 443, false},
	}
	for _, c := range cases {
		if got := n.AllowHost(c.host, c.port); got != c.want {
			t.Errorf("%s:%d = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestPathMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"/repos/**", "/repos/a/b/c", true},
		{"/repos/**", "/repos", true},
		{"/repos/*/issues", "/repos/a/issues", true},
		{"/repos/*/issues", "/repos/a/b/issues", false},
		{"/repos/imohiyoko/**", "/repos/other/x", false},
		{"/repos/**", "/repos/../admin", false},
		{"/repos/**", "/repos/./x", false},
		{"/v1/messages", "/v1/messages?x=1", true},
		{"/v1/messages", "/v1/messages/x", false},
		{"/a/*.json", "/a/b.json", true},
		{"/a/*.json", "/a/b.txt", false},
		{"/**/raw", "/x/y/raw", true},
	}
	for _, c := range cases {
		if got := PathMatch(c.pat, c.path); got != c.want {
			t.Errorf("PathMatch(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}
