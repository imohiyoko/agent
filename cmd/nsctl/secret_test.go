package main

import (
	"io"
	"os"
	"testing"
)

func TestSecretName(t *testing.T) {
	for _, n := range []string{"research/claude-oauth", "a/x.y_z"} {
		if !secretNameRe.MatchString(n) {
			t.Errorf("%s must be accepted", n)
		}
	}
	for _, n := range []string{"claude", "Research/x", "a/", "a/b/c", "../x"} {
		if secretNameRe.MatchString(n) {
			t.Errorf("%s must be rejected", n)
		}
	}
}

func TestReadSecretFromPipe(t *testing.T) {
	read := func(in string) (string, error) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = w.WriteString(in); w.Close() }()
		defer r.Close()
		return readSecret(r, io.Discard, "a/x")
	}
	if v, err := read("tok\n"); err != nil || v != "tok" {
		t.Errorf("got %q, %v", v, err)
	}
	if v, err := read("tok"); err != nil || v != "tok" {
		t.Errorf("no newline: got %q, %v", v, err)
	}
	if _, err := read(""); err == nil {
		t.Error("empty input must fail")
	}
}
