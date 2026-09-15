package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepositoryRootFixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
	return dir
}

func TestRepositoryRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the git binary")
	}
	t.Run("inside a repository resolves to its root", func(t *testing.T) {
		root := initRepositoryRootFixture(t, "repo-root")
		nested := filepath.Join(root, "internal", "pkg")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := RepositoryRoot(nested); got != root {
			t.Fatalf("RepositoryRoot(%q) = %q, want %q", nested, got, root)
		}
	})
	t.Run("outside a repository resolves to empty", func(t *testing.T) {
		plain := t.TempDir()
		if got := RepositoryRoot(plain); got != "" {
			t.Fatalf("RepositoryRoot(%q) = %q, want empty", plain, got)
		}
	})
	t.Run("empty directory resolves to empty", func(t *testing.T) {
		if got := RepositoryRoot(""); got != "" {
			t.Fatalf("RepositoryRoot(\"\") = %q, want empty", got)
		}
	})
}
