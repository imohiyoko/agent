package secrets

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFileSetNamesDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "secrets.yaml")
	f := File{Path: path}

	// A missing file is created with mode 0600.
	if err := f.Set("a/claude", "yes"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st.Mode().Perm(), err)
	}
	if v, err := f.Get("a/claude"); err != nil || v != "yes" {
		t.Fatalf("Get = %q, %v (a value like yes must stay a string)", v, err)
	}

	// Comments and other entries are kept; an existing entry is replaced.
	if err := os.WriteFile(path, []byte("# keep me\na/github: ghp_x\na/claude: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("a/claude", "new"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("b/x", "1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# keep me") {
		t.Errorf("comment lost:\n%s", b)
	}
	if v, _ := f.Get("a/claude"); v != "new" {
		t.Errorf("a/claude = %q", v)
	}
	names, err := f.Names()
	if err != nil || !reflect.DeepEqual(names, []string{"a/claude", "a/github", "b/x"}) {
		t.Errorf("Names = %v, %v", names, err)
	}

	if err := f.Delete("a/github"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("a/github"); err == nil {
		t.Error("deleted secret still readable")
	}
	if err := f.Delete("a/github"); err == nil {
		t.Error("deleting a missing secret must fail")
	}
}

func TestFileRejectsNonMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(path, []byte("- a\n- b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (File{Path: path}).Set("a/x", "v"); err == nil {
		t.Fatal("a list must not be rewritten")
	}
}
