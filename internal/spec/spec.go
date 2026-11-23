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

// MCPServer is an MCP server behind the gateway: either a stdio server the
// gateway starts (Command), or a remote Streamable HTTP server reached
// through one of this namespace's credential routes (Credential + Path).
type MCPServer struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	// Credential names a credential route of this namespace. The gateway
	// sends the agent's MCP messages to it, so the route's rules, secret and
	// audit apply. Path is appended to the route (default "/mcp").
	Credential     string            `yaml:"credential"`
	Path           string            `yaml:"path"`
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
	Name string `yaml:"name"`
	// Command runs the agent on one prompt (nsctl run, the API).
	Command []string `yaml:"command"`
	// Interactive runs the agent in a person's terminal (nsctl shell), for
	// example ["claude"]. It may use {mcp_config} but not {prompt}. Empty
	// means the agent has no interactive mode.
	Interactive []string          `yaml:"interactive"`
	Workdir     string            `yaml:"workdir"`
	Env         map[string]string `yaml:"env"`
	// Home is the agent's HOME (default: the nsctl user's home). Give each
	// namespace its own, inside filesystem.writable, so tools' settings and
	// caches (~/.claude, ~/.npm ...) are not shared between namespaces.
	Home string `yaml:"home"`
	// Timeout stops a run after this long ("30m", "2h"). Empty means the
	// server's default.
	Timeout string `yaml:"timeout"`

	// TimeoutDur is Timeout parsed (0 = server default).
	TimeoutDur time.Duration `yaml:"-"`
}

// Placeholders that may appear in Agent.Command and Agent.Interactive.
// {mcp_url} and {creds_url} may also appear in Agent.Env values.
const (
	// PromptPlaceholder is the prompt, as one whole argument.
	PromptPlaceholder = "{prompt}"
	// MCPConfigPlaceholder is the path of a JSON file in Claude Code's
	// --mcp-config format, listing the namespace's MCP servers.
	MCPConfigPlaceholder = "{mcp_config}"
	// MCPURLPlaceholder is the MCP gateway's base URL as the sandbox sees it,
	// for example http://127.0.0.1:3129/mcp. Server <name> is at
	// {mcp_url}/<name> (Streamable HTTP). For agents that do not read
	// Claude Code's format (Codex CLI, Gemini CLI, ...).
	MCPURLPlaceholder = "{mcp_url}"
	// CredsURLPlaceholder is the base URL of the namespace's credential
	// routes on loopback, for example http://127.0.0.1:3128/creds. Route
	// <name> is at {creds_url}/<name>, the same as
	// http://<name>.creds.internal. For agents that take plain http only on
	// loopback (Gemini CLI) or do not use HTTP_PROXY for their API.
	CredsURLPlaceholder = "{creds_url}"
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
		switch {
		case len(s.Command) == 0 && s.Credential == "":
			bad("%s: set command (stdio server) or credential (remote server)", f)
		case len(s.Command) > 0 && s.Credential != "":
			bad("%s: set command or credential, not both", f)
		case s.Credential != "":
			if ns.Credential(s.Credential) == nil {
				bad("%s: credential %q is not a credential route of this namespace", f, s.Credential)
			}
			if s.Path == "" {
				ns.MCP.Servers[i].Path = "/mcp"
			} else if !strings.HasPrefix(s.Path, "/") {
				bad("%s: path %q must start with /", f, s.Path)
			}
			if len(s.Env) > 0 {
				bad("%s: env is only for stdio servers", f)
			}
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
			if looksLikeKey(v.Value) {
				bad("%s.env.%s: looks like a real key; use {secret: %s/<name>}", f, k, ns.Name)
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
		for _, arg := range a.Interactive {
			if strings.Contains(arg, PromptPlaceholder) {
				bad("%s: interactive must not use %s (the person types the prompts)", f, PromptPlaceholder)
			}
		}
		if len(ns.MCP.Servers) == 0 {
			all := append(append([]string{}, a.Command...), a.Interactive...)
			for _, v := range a.Env {
				all = append(all, v)
			}
			for _, arg := range all {
				if strings.Contains(arg, MCPConfigPlaceholder) || strings.Contains(arg, MCPURLPlaceholder) {
					bad("%s: %s and %s need mcp.servers in this namespace", f, MCPConfigPlaceholder, MCPURLPlaceholder)
					break
				}
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
		for k, v := range a.Env {
			if !envRe.MatchString(k) {
				bad("%s.env: %q is not a valid variable name", f, k)
			}
			// Agents never get secrets: they reach APIs through credential
			// routes, which add the key outside the sandbox.
			if looksLikeKey(v) {
				bad("%s.env.%s: looks like a real key; agents get keys only through credentials (routes)", f, k)
			}
		}
		if a.Home != "" {
			ns.Agents[i].Home = expand(a.Home, home)
			if !ns.isWritable(ns.Agents[i].Home) {
				bad("%s: home %q must be inside filesystem.writable", f, a.Home)
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

// keyPrefixes start well-known API keys and tokens. A declaration that holds
// one is refused, since declarations are kept in git (invariant: no secret
// values in declarations).
var keyPrefixes = []string{
	"sk-ant-",                             // Anthropic API keys and Claude OAuth tokens
	"sk-proj-",                            // OpenAI
	"ghp_", "gho_", "ghs_", "github_pat_", // GitHub
	"xoxb-", "xoxp-", // Slack
	"AKIA", // AWS access key id
}

func looksLikeKey(v string) bool {
	for _, p := range keyPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
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

// GatewayOnly reports whether the credential route is used by a remote MCP
// server. Such routes are only usable by the MCP gateway, so an agent cannot
// skip the gateway's tool checks by calling the route itself.
func (ns *Namespace) GatewayOnly(route string) bool {
	for _, s := range ns.MCP.Servers {
		if s.Credential == route {
			return true
		}
	}
	return false
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
