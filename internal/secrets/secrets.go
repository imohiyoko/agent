// Package secrets resolves secret names ("<namespace>/<name>") to values.
//
// Values live outside the repository, in a YAML file with mode 0600, or in
// environment variables of the nsctl process. Values are never logged.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source looks up secret values.
type Source interface {
	Get(name string) (string, error)
}

// File reads a YAML map of "<namespace>/<name>: <value>". A value of the
// form "env:VAR" is read from the environment of nsctl instead.
type File struct {
	Path string
}

// Get returns the value of the secret.
func (f File) Get(name string) (string, error) {
	st, err := os.Stat(f.Path)
	if err != nil {
		return "", fmt.Errorf("secrets file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secrets file %s must not be readable by others (chmod 600)", f.Path)
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}
	m := map[string]string{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("secrets file %s: %w", f.Path, err)
	}
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("secret %q is not set", name)
	}
	if env, ok := strings.CutPrefix(v, "env:"); ok {
		v = os.Getenv(env)
		if v == "" {
			return "", fmt.Errorf("secret %q refers to an empty environment variable", name)
		}
	}
	return v, nil
}

// Map is an in-memory source, for tests.
type Map map[string]string

// Get returns the value of the secret.
func (m Map) Get(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", errors.New("secret is not set: " + name)
	}
	return v, nil
}

// Names returns the secret names in the file, sorted, without their values.
// A missing file has no names.
func (f File) Names() ([]string, error) {
	doc, err := f.load()
	if err != nil {
		return nil, err
	}
	var names []string
	m := mapping(doc)
	for i := 0; i+1 < len(m.Content); i += 2 {
		names = append(names, m.Content[i].Value)
	}
	sort.Strings(names)
	return names, nil
}

// Set adds or replaces one secret. The rest of the file, comments included,
// is kept. The file is created with mode 0600 if it does not exist, and is
// replaced in one step (write to a temporary file, then rename).
func (f File) Set(name, value string) error {
	doc, err := f.load()
	if err != nil {
		return err
	}
	m := mapping(doc)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			m.Content[i+1] = scalar(value)
			return f.save(doc)
		}
	}
	m.Content = append(m.Content, scalar(name), scalar(value))
	return f.save(doc)
}

// Delete removes one secret. Removing a secret that is not set is an error.
func (f File) Delete(name string) error {
	doc, err := f.load()
	if err != nil {
		return err
	}
	m := mapping(doc)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == name {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return f.save(doc)
		}
	}
	return fmt.Errorf("secret %q is not set", name)
}

// load reads the file as a YAML document. A missing file is an empty one.
func (f File) load() (*yaml.Node, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("secrets file %s: %w", f.Path, err)
	}
	if doc.Kind == 0 { // empty file
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("secrets file %s: must be a map of name: value", f.Path)
	}
	return &doc, nil
}

func (f File) save(doc *yaml.Node) error {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func mapping(doc *yaml.Node) *yaml.Node { return doc.Content[0] }

// scalar is a plain string node. Values are always quoted so that a token
// such as "yes" or "0123" stays a string.
func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}
