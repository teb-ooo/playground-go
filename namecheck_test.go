package playground_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoOldNameInSources guards the factory to playground rename: the old word must not reappear in Go sources,
// templates or docs. The allowlist is empty on purpose; this file is the only place that spells the word.
func TestNoOldNameInSources(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.IsDir() || p == "namecheck_test.go" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, l := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(l), "factory") {
				t.Errorf("%s:%d still names the old product: %s", p, i+1, strings.TrimSpace(l))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
