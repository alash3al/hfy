package dbtest

import "os/exec"

// Verdict is the part of testing.TB that NeedNode uses.
type Verdict interface {
	Helper()
	Fatalf(string, ...any)
	Skipf(string, ...any)
}

// NeedNode ends a test that runs the embedded JS under node when node is
// missing: it fails under HFY_TEST_REQUIRE_DB (CI), and skips otherwise.
func NeedNode(t Verdict) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if Required() {
			t.Fatalf("node is required to run the embedded JS: %v", err)
		}
		t.Skipf("no node in PATH: %v", err)
	}
}
