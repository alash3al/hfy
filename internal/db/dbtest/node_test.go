package dbtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alash3al/hfy/internal/db/dbtest"
)

// fakeT records what NeedNode did.
type fakeT struct{ failed, skipped bool }

func (*fakeT) Helper()                 {}
func (f *fakeT) Fatalf(string, ...any) { f.failed = true; panic(f) } // like t.Fatalf, it does not return
func (f *fakeT) Skipf(string, ...any)  { f.skipped = true }

func TestNeedNode(t *testing.T) {
	empty := t.TempDir()
	withNode := t.TempDir()
	if err := os.WriteFile(filepath.Join(withNode, "node"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path, require string
		failed, skipped     bool
	}{
		{"missing, required", empty, "1", true, false},
		{"missing, not required", empty, "", false, true},
		{"present, required", withNode, "1", false, false},
		{"present, not required", withNode, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", c.path)
			t.Setenv("HFY_TEST_REQUIRE_DB", c.require)
			var f fakeT
			func() {
				defer func() { recover() }()
				dbtest.NeedNode(&f)
			}()
			if f.failed != c.failed || f.skipped != c.skipped {
				t.Fatalf("failed=%v skipped=%v, want failed=%v skipped=%v", f.failed, f.skipped, c.failed, c.skipped)
			}
		})
	}
}
