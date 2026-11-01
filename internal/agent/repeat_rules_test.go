package agent

import (
	"testing"

	"github.com/alash3al/hfy/internal/types"
)

func TestRepeatVerdict(t *testing.T) {
	for n, want := range map[int]verdict{0: verdictOK, 1: verdictOK, 2: verdictOK,
		3: verdictWarn, 4: verdictWarn, 5: verdictWarn, 6: verdictAbort, 7: verdictAbort} {
		if got := repeatVerdict(n); got != want {
			t.Errorf("repeatVerdict(%d) = %v, want %v", n, got, want)
		}
	}
}

// The key is the same however the store writes the arguments back, and
// different calls keep different keys.
func TestCallKeyIgnoresByteForm(t *testing.T) {
	a := callKey("files.echo", []byte(`{"b":1,"a":"x"}`))
	if b := callKey("files.echo", []byte(`{"a": "x", "b": 1}`)); a != b {
		t.Errorf("reordered keys: %q != %q", a, b)
	}
	for _, other := range []string{
		callKey("files.echo", []byte(`{"a":"y","b":1}`)),
		callKey("files.write", []byte(`{"b":1,"a":"x"}`)),
	} {
		if other == a {
			t.Errorf("different call shares a key: %q", a)
		}
	}
}

// Seeding a resumed turn counts its recorded calls exactly as the live path
// counted them, whatever order the stored arguments come back in.
func TestSeedCountsMatchTheLiveKeys(t *testing.T) {
	l := &loop{counts: map[string]int{}}
	l.seedCounts([]types.ToolCall{
		{Name: "hfy.now", Arguments: []byte(`{"b":1,"a":2}`)},
		{Name: "hfy.now", Arguments: []byte(`{"a": 2, "b": 1}`)},
	})
	if n := l.counts[callKey("hfy.now", []byte(`{"a":2,"b":1}`))]; n != 2 {
		t.Fatalf("seeded count = %d, want 2", n)
	}
}
