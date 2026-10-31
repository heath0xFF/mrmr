package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeDatabaseOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mrmr.db")
	first, err := lockRuntimeDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockRuntimeDB(path); err == nil {
		second.Close()
		t.Fatal("second runtime acquired the database")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := path + ".alias"
	if err := os.Symlink(path, alias); err == nil {
		if second, err := lockRuntimeDB(alias); err == nil {
			second.Close()
			t.Fatal("symlink bypassed database ownership")
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := lockRuntimeDB(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
