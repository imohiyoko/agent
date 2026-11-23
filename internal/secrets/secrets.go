// Package secrets resolves secret names ("<namespace>/<name>") to values.
//
// Values live outside the repository, in a YAML file with mode 0600, or in
// environment variables of the nsctl process. Values are never logged.
package secrets

import (
	"errors"
	"fmt"
	"os"
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
