package config

import (
	"os"
	"testing"
)

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s perm = %v, want %v", path, got, want)
	}
}
