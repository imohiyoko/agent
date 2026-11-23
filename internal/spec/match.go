package spec

import (
	"strings"
)

// Match reports whether host:port matches the rule. "*.example.com" matches
// "a.example.com" and "a.b.example.com" but not "example.com".
func (r HostRule) Match(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ok := false
	if strings.HasPrefix(r.Host, "*.") {
		ok = strings.HasSuffix(host, r.Host[1:]) && len(host) > len(r.Host)-1
	} else {
		ok = host == r.Host
	}
	if !ok {
		return false
	}
	for _, p := range r.Ports {
		if p == port {
			return true
		}
	}
	return false
}

// AllowHost reports whether the namespace may connect to host:port. Deny wins
// over allow; nothing matching means deny.
func (n Network) AllowHost(host string, port int) bool {
	for _, r := range n.Deny {
		if r.Match(host, port) {
			return false
		}
	}
	for _, r := range n.Allow {
		if r.Match(host, port) {
			return true
		}
	}
	return false
}

// Allow reports whether the credential route permits method on path.
func (c *Credential) Allow(method, path string) bool {
	method = strings.ToUpper(method)
	for _, r := range c.Rules {
		mOK := false
		for _, m := range r.Methods {
			if m == method {
				mOK = true
				break
			}
		}
		if !mOK {
			continue
		}
		for _, p := range r.Paths {
			if PathMatch(p, path) {
				return true
			}
		}
	}
	return false
}

// PathMatch matches a URL path against a glob: "*" matches within one
// segment, "**" (a whole segment) matches zero or more segments. Paths with
// "." or ".." segments never match, so a rule cannot be escaped with them.
func PathMatch(pattern, path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	ps := strings.Split(strings.Trim(path, "/"), "/")
	for _, s := range ps {
		if s == "." || s == ".." {
			return false
		}
	}
	return matchSegs(strings.Split(strings.Trim(pattern, "/"), "/"), ps)
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 || !matchSeg(pat[0], segs[0]) {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0 || (len(segs) == 1 && segs[0] == "")
}

func matchSeg(pat, s string) bool {
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts); i++ {
		if i == len(parts)-1 {
			return strings.HasSuffix(s, parts[i])
		}
		j := strings.Index(s, parts[i])
		if j < 0 {
			return false
		}
		s = s[j+len(parts[i]):]
	}
	return true
}
