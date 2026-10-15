package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/heath0xff/mrmr/internal/superhuman"
)

// Invalid CLI input must fail before networking, and checking an unsigned-in
// connection must not launch a browser or require a flow/model configuration.
func TestAuthCLI(t *testing.T) {
	for _, args := range [][]string{
		nil, {"login"}, {"login", "arbitrary-server"}, {"send", "superhuman"},
		{"login", "superhuman", "-callback-port", "-1", "-state-dir", t.TempDir()},
		{"check", "superhuman", "unexpected"},
		{"check", "superhuman", "-callback-port", "1234"},
		{"check", "superhuman", "-state-dir", "relative"},
	} {
		var out bytes.Buffer
		if err := runAuth(t.Context(), args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("args=%v err=%v output=%q", args, err, out.String())
		}
	}
	var out bytes.Buffer
	err := runAuth(t.Context(), []string{"check", "superhuman", "-state-dir", filepath.Join(t.TempDir(), "private")}, &out)
	if !errors.Is(err, superhuman.ErrLoginRequired) || out.Len() != 0 {
		t.Fatalf("unsigned-in check: %v output=%q", err, out.String())
	}
}
