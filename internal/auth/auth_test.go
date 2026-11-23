package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imohiyoko/agent/internal/spec"
)

func TestTokenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	st := NewStore(path)
	tok, value, err := st.Create("alice", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(value, TokenPrefix) {
		t.Fatalf("value %q has no prefix", value)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), value) {
		t.Fatal("the token value must not be stored")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	if p, _ := st.Authenticate(value); p != "alice" {
		t.Fatalf("authenticate = %q", p)
	}
	for _, bad := range []string{"", "nsk_", value + "x", "Bearer " + value, strings.TrimPrefix(value, TokenPrefix)} {
		if p, _ := st.Authenticate(bad); p != "" {
			t.Errorf("%q authenticated as %q", bad, p)
		}
	}
	if err := st.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.Authenticate(value); p != "" {
		t.Fatal("revoked token still works")
	}
	if _, _, err := st.Create("Bad Name", ""); err == nil {
		t.Fatal("invalid principal accepted")
	}
}

func TestAuthorizer(t *testing.T) {
	ns := &spec.Namespace{Name: "a", Members: []spec.Member{
		{Principal: "v", Role: spec.RoleViewer},
		{Principal: "o", Role: spec.RoleOperator},
		{Principal: "ad", Role: spec.RoleAdmin},
	}}
	z := &Authorizer{Admins: map[string]bool{"root": true}}
	cases := []struct {
		who, act string
		want     bool
	}{
		{"v", ActNamespaceRead, true},
		{"v", ActRunRead, true},
		{"v", ActRunCreate, false},
		{"v", ActRunStop, false},
		{"o", ActRunCreate, true},
		{"o", ActRunStop, true},
		{"o", ActAuditRead, false},
		{"ad", ActAuditRead, true},
		{"root", ActAuditRead, true},
		{"stranger", ActNamespaceRead, false},
		{"", ActNamespaceRead, false},
		{"ad", "unknown.action", false},
	}
	for _, c := range cases {
		if got := z.Allowed(c.who, ns, c.act); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.who, c.act, got, c.want)
		}
	}
	if z.Allowed("root", nil, ActNamespaceRead) {
		t.Error("nil namespace must be denied")
	}
}

func TestTokenExpiry(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "tokens.json"))
	_, live, _ := st.CreateTTL("alice", "", time.Hour)
	_, dead, _ := st.CreateTTL("alice", "", time.Nanosecond)
	time.Sleep(10 * time.Millisecond)
	if p, _ := st.Authenticate(live); p != "alice" {
		t.Error("live token rejected")
	}
	if p, _ := st.Authenticate(dead); p != "" {
		t.Error("expired token accepted")
	}
	if _, _, err := st.CreateTTL("alice", "", -time.Hour); err == nil {
		t.Error("negative ttl accepted")
	}
}
