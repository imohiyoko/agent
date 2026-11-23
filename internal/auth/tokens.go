// Package auth authenticates API callers (people and systems, never agents)
// and decides what they may do in each namespace.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imohiyoko/agent/internal/spec"
)

// TokenPrefix starts every token, so leaked tokens are easy to find.
const TokenPrefix = "nsk_"

// Token is a stored token. The value itself is never stored.
type Token struct {
	ID        string    `json:"id"`
	Principal string    `json:"principal"`
	Note      string    `json:"note,omitempty"`
	Hash      string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

// Store keeps token hashes in a JSON file with mode 0600.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) load() ([]Token, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ts []Token
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return ts, nil
}

func (s *Store) save(ts []Token) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create makes a token for principal and returns its value. The value is
// shown once and cannot be recovered.
func (s *Store) Create(principal, note string) (Token, string, error) {
	if !spec.IsPrincipalName(principal) {
		return Token{}, "", fmt.Errorf("principal %q is not a valid name", principal)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return Token{}, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", err
	}
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return Token{}, "", err
	}
	value := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	t := Token{
		ID:        hex.EncodeToString(idb),
		Principal: principal,
		Note:      note,
		Hash:      hash(value),
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	ts = append(ts, t)
	if err := s.save(ts); err != nil {
		return Token{}, "", err
	}
	return t, value, nil
}

// List returns the stored tokens (without values), oldest first.
func (s *Store) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].CreatedAt.Before(ts[j].CreatedAt) })
	return ts, err
}

// Revoke deletes the token with that id.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return err
	}
	out := ts[:0]
	found := false
	for _, t := range ts {
		if t.ID == id {
			found = true
			continue
		}
		out = append(out, t)
	}
	if !found {
		return fmt.Errorf("no token with id %q", id)
	}
	return s.save(out)
}

// Authenticate returns the principal for a token value, or "" if the value is
// unknown. The file is read on every call so revoking takes effect at once.
func (s *Store) Authenticate(value string) (string, error) {
	if !strings.HasPrefix(value, TokenPrefix) {
		return "", nil
	}
	h := hash(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		// Comparing hashes, not values: the time taken says nothing useful
		// about the secret.
		if t.Hash == h {
			return t.Principal, nil
		}
	}
	return "", nil
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
