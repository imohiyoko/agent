// Package spec reads and validates namespace declarations.
//
// It knows nothing about sandboxes, proxies or OpenShell. Everything that
// keeps the invariants in docs/design.md ("default deny", "no crossing
// namespaces", "no secret values") is checked here, before anything runs.
package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Roles a principal can hold in a namespace.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	hostRe   = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// CredsSuffix is the host suffix agents use to reach a credential route.
const CredsSuffix = ".creds.internal"

// Namespace is one namespace declaration (namespaces/<name>.yaml).
type Namespace struct {
	Version     int          `yaml:"version"`
	Name        string       `yaml:"name"`
	Members     []Member     `yaml:"members"`
	Network     Network      `yaml:"network"`
	Credentials []Credential `yaml:"credentials"`
	Filesystem  Filesystem   `yaml:"filesystem"`
	MCP         MCP          `yaml:"mcp"`
	Agents      []Agent      `yaml:"agents"`

	// File is where the declaration was read from (not part of the YAML).
	File string `yaml:"-"`
}

// Member gives a principal a role in this namespace.
type Member struct {
	Principal string `yaml:"principal"`
	Role      string `yaml:"role"`
}

// Network lists the destinations the namespace may reach. Anything not
// allowed is denied. Deny wins over allow.
type Network struct {
	Allow []HostRule `yaml:"allow"`
	Deny  []HostRule `yaml:"deny"`
}

// HostRule matches a host (exact, or "*.example.com" for subdomains) and ports.
type HostRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports"`
}

// Credential is a credential route: agents send plain HTTP to
// http://<name>.creds.internal/..., the proxy checks Rules, adds the header
// and forwards to Upstream over TLS. The agent never sees the value.
type Credential struct {
	Name     string     `yaml:"name"`
	Upstream string     `yaml:"upstream"`
	Header   string     `yaml:"header"`
	Format   string     `yaml:"format"`
	Secret   string     `yaml:"secret"`
	Rules    []HTTPRule `yaml:"rules"`
}

// HTTPRule allows some methods on some paths. Paths are globs where "*"
// matches within one segment and "**" matches any number of segments.
type HTTPRule struct {
	Methods []string `yaml:"methods"`
	Paths   []string `yaml:"paths"`
}

// Filesystem limits writes and hides paths from the sandbox.
type Filesystem struct {
	Writable []string `yaml:"writable"`
	DenyRead []string `yaml:"deny_read"`
}

// MCP lists the MCP servers the namespace may use through the gateway.
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is a stdio MCP server started by the gateway.
type MCPServer struct {
	Name           string            `yaml:"name"`
	Command        []string          `yaml:"command"`
	Env            map[string]EnvVal `yaml:"env"`
	Tools          []string          `yaml:"tools"`
	AllowResources bool              `yaml:"allow_resources"`
	AllowPrompts   bool              `yaml:"allow_prompts"`
}

// EnvVal is either a literal value or a reference to a secret.
type EnvVal struct {
	Value  string `yaml:"value"`
	Secret string `yaml:"secret"`
}

// UnmarshalYAML accepts both `KEY: literal` and `KEY: {secret: ns/name}`.
func (e *EnvVal) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.Value = n.Value
		return nil
	}
	type plain EnvVal
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*e = EnvVal(p)
	return nil
}

// Agent is a command the API may start in this namespace.
type Agent struct {
	Name    string            `yaml:"name"`
	Command []string          `yaml:"command"`
	Workdir string            `yaml:"workdir"`
	Env     map[string]string `yaml:"env"`
	// Timeout stops a run after this long ("30m", "2h"). Empty means the
	// server's default.
	Timeout string `yaml:"timeout"`

	// TimeoutDur is Timeout parsed (0 = server default).
	TimeoutDur time.Duration `yaml:"-"`
}

// Placeholders that may appear in Agent.Command.
const (
	PromptPlaceholder    = "{prompt}"
	MCPConfigPlaceholder = "{mcp_config}"
)

// Set is every namespace declaration, by name.
type Set struct {
	Namespaces map[string]*Namespace
}

// Names returns the namespace names in sorted order.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.Namespaces))
	for n := range s.Namespaces {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadDir reads every *.yaml / *.yml file in dir as a namespace and validates
// all of them. Any error stops the load: a half-valid set is never returned.
func LoadDir(dir string, home string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	set := &Set{Namespaces: map[string]*Namespace{}}
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ns, err := LoadFile(path, home)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want := strings.TrimSuffix(e.Name(), ext); ns.Name != want {
			errs = append(errs, fmt.Errorf("%s: name %q must match the file name %q", path, ns.Name, want))
			continue
		}
		set.Namespaces[ns.Name] = ns
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return set, nil
}

// LoadFile reads and validates one namespace file. Unknown fields are errors,
// so a typo cannot silently drop a restriction.
func LoadFile(path string, home string) (*Namespace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var ns Namespace
	if err := dec.Decode(&ns); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ns.File = path
	if err := ns.Validate(home); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ns, nil
}

// Validate checks the declaration and expands "~" in paths. It returns every
// problem found, not just the first.
func (ns *Namespace) Validate(home string) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if ns.Version != 1 {
		bad("version must be 1")
	}
	if !nameRe.MatchString(ns.Name) {
		bad("name %q must match %s", ns.Name, nameRe)
	}

	seen := map[string]bool{}
	for i, m := range ns.Members {
		if !nameRe.MatchString(m.Principal) {
			bad("members[%d]: principal %q must match %s", i, m.Principal, nameRe)
		}
		switch m.Role {
		case RoleViewer, RoleOperator, RoleAdmin:
		default:
			bad("members[%d]: role %q must be viewer, operator or admin", i, m.Role)
		}
		if seen[m.Principal] {
			bad("members[%d]: principal %q is listed twice", i, m.Principal)
		}
		seen[m.Principal] = true
	}

	checkHosts := func(field string, rules []HostRule) {
		for i, r := range rules {
			r.Host = strings.ToLower(r.Host)
			rules[i].Host = r.Host
			if r.Host == "" || r.Host == "*" || strings.Contains(strings.TrimPrefix(r.Host, "*."), "*") || !hostRe.MatchString(r.Host) {
				bad("%s[%d]: host %q must be a host name or *.domain (a bare * is not allowed)", field, i, r.Host)
			}
			if strings.HasSuffix(r.Host, CredsSuffix) {
				bad("%s[%d]: host %q is reserved for credential routes", field, i, r.Host)
			}
			if len(r.Ports) == 0 {
				bad("%s[%d]: ports must be listed", field, i)
			}
			for _, p := range r.Ports {
				if p < 1 || p > 65535 {
					bad("%s[%d]: port %d is out of range", field, i, p)
				}
			}
		}
	}
	checkHosts("network.allow", ns.Network.Allow)
	checkHosts("network.deny", ns.Network.Deny)

	secretPrefix := ns.Name + "/"
	checkSecret := func(field, s string) {
		if !strings.HasPrefix(s, secretPrefix) || len(s) == len(secretPrefix) {
			bad("%s: secret %q must belong to this namespace (%s<name>)", field, s, secretPrefix)
		}
	}

	seen = map[string]bool{}
	for i, c := range ns.Credentials {
		f := fmt.Sprintf("credentials[%d]", i)
		if !nameRe.MatchString(c.Name) {
			bad("%s: name %q must match %s", f, c.Name, nameRe)
		}
		if seen[c.Name] {
			bad("%s: name %q is listed twice", f, c.Name)
		}
		seen[c.Name] = true
		if !strings.HasPrefix(c.Upstream, "https://") {
			bad("%s: upstream must start with https://", f)
		}
		if !headerRe.MatchString(c.Header) {
			bad("%s: header %q is not a valid header name", f, c.Header)
		}
		if c.Format != "" && strings.Count(c.Format, "{value}") != 1 {
			bad("%s: format must contain {value} exactly once", f)
		}
		checkSecret(f, c.Secret)
		if len(c.Rules) == 0 {
			bad("%s: rules must be listed (nothing is allowed by default)", f)
		}
		for j, r := range c.Rules {
			if len(r.Methods) == 0 || len(r.Paths) == 0 {
				bad("%s.rules[%d]: methods and paths must be listed", f, j)
			}
			for k, m := range r.Methods {
				m = strings.ToUpper(m)
				c.Rules[j].Methods[k] = m
				if m == "*" {
					bad("%s.rules[%d]: method * is not allowed; list the methods", f, j)
				}
			}
			for _, p := range r.Paths {
				if !strings.HasPrefix(p, "/") {
					bad("%s.rules[%d]: path %q must start with /", f, j, p)
				}
			}
		}
	}

	for i, p := range ns.Filesystem.Writable {
		ns.Filesystem.Writable[i] = expand(p, home)
		if !filepath.IsAbs(ns.Filesystem.Writable[i]) {
			bad("filesystem.writable[%d]: %q must be an absolute path or start with ~/", i, p)
		}
		if clean := filepath.Clean(ns.Filesystem.Writable[i]); clean == "/" || clean == home {
			bad("filesystem.writable[%d]: %q is too broad", i, p)
		}
	}
	for i, p := range ns.Filesystem.DenyRead {
		ns.Filesystem.DenyRead[i] = expand(p, home)
		if !filepath.IsAbs(ns.Filesystem.DenyRead[i]) {
			bad("filesystem.deny_read[%d]: %q must be an absolute path or start with ~/", i, p)
		}
	}

	seen = map[string]bool{}
	for i, s := range ns.MCP.Servers {
		f := fmt.Sprintf("mcp.servers[%d]", i)
		if !nameRe.MatchString(s.Name) {
			bad("%s: name %q must match %s", f, s.Name, nameRe)
		}
		if seen[s.Name] {
			bad("%s: name %q is listed twice", f, s.Name)
		}
		seen[s.Name] = true
		if len(s.Command) == 0 {
			bad("%s: command must be set", f)
		}
		if len(s.Tools) == 0 {
			bad("%s: tools must be listed (nothing is allowed by default)", f)
		}
		for _, t := range s.Tools {
			if t == "" || strings.Contains(t, "*") {
				bad("%s: tool %q must be a full tool name (no wildcards)", f, t)
			}
		}
		for k, v := range s.Env {
			if !envRe.MatchString(k) {
				bad("%s.env: %q is not a valid variable name", f, k)
			}
			if v.Secret != "" && v.Value != "" {
				bad("%s.env.%s: set value or secret, not both", f, k)
			}
			if v.Secret != "" {
				checkSecret(f+".env."+k, v.Secret)
			}
		}
	}

	seen = map[string]bool{}
	for i, a := range ns.Agents {
		f := fmt.Sprintf("agents[%d]", i)
		if !nameRe.MatchString(a.Name) {
			bad("%s: name %q must match %s", f, a.Name, nameRe)
		}
		if seen[a.Name] {
			bad("%s: name %q is listed twice", f, a.Name)
		}
		seen[a.Name] = true
		if len(a.Command) == 0 {
			bad("%s: command must be set", f)
		}
		for _, arg := range a.Command {
			if arg != PromptPlaceholder && strings.Contains(arg, PromptPlaceholder) {
				bad("%s: %s must be a whole argument, not part of one", f, PromptPlaceholder)
			}
		}
		if a.Workdir == "" {
			bad("%s: workdir must be set", f)
			continue
		}
		ns.Agents[i].Workdir = expand(a.Workdir, home)
		if !ns.isWritable(ns.Agents[i].Workdir) {
			bad("%s: workdir %q must be inside filesystem.writable", f, a.Workdir)
		}
		for k := range a.Env {
			if !envRe.MatchString(k) {
				bad("%s.env: %q is not a valid variable name", f, k)
			}
		}
		if a.Timeout != "" {
			d, err := time.ParseDuration(a.Timeout)
			if err != nil || d <= 0 {
				bad("%s: timeout %q must be a positive duration like 30m", f, a.Timeout)
			}
			ns.Agents[i].TimeoutDur = d
		}
	}
	return errors.Join(errs...)
}

func (ns *Namespace) isWritable(p string) bool {
	p = filepath.Clean(p)
	for _, w := range ns.Filesystem.Writable {
		w = filepath.Clean(w)
		if p == w || strings.HasPrefix(p, w+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Agent returns the agent with that name, or nil.
func (ns *Namespace) Agent(name string) *Agent {
	for i := range ns.Agents {
		if ns.Agents[i].Name == name {
			return &ns.Agents[i]
		}
	}
	return nil
}

// Credential returns the credential route with that name, or nil.
func (ns *Namespace) Credential(name string) *Credential {
	for i := range ns.Credentials {
		if ns.Credentials[i].Name == name {
			return &ns.Credentials[i]
		}
	}
	return nil
}

// MCPServer returns the MCP server with that name, or nil.
func (ns *Namespace) MCPServer(name string) *MCPServer {
	for i := range ns.MCP.Servers {
		if ns.MCP.Servers[i].Name == name {
			return &ns.MCP.Servers[i]
		}
	}
	return nil
}

// RoleOf returns the principal's role in this namespace, or "".
func (ns *Namespace) RoleOf(principal string) string {
	for _, m := range ns.Members {
		if m.Principal == principal {
			return m.Role
		}
	}
	return ""
}

func expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// IsPrincipalName reports whether s is a valid principal name.
func IsPrincipalName(s string) bool { return nameRe.MatchString(s) }
