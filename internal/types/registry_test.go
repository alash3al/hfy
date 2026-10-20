package types

import (
	"encoding/json"
	"strings"
	"testing"
)

type renamed struct {
	Body string `json:"body"`
}

func (renamed) EventType() EventType { return "test.renamed" }
func (renamed) Validate() error      { return nil }

func TestUpgradeChain(t *testing.T) {
	// v1 {"text"} -> v2 {"msg"} -> v3 {"body"}
	register[renamed](false,
		func(b json.RawMessage) (json.RawMessage, error) {
			return []byte(strings.Replace(string(b), `"text"`, `"msg"`, 1)), nil
		},
		func(b json.RawMessage) (json.RawMessage, error) {
			return []byte(strings.Replace(string(b), `"msg"`, `"body"`, 1)), nil
		})
	defer delete(registry, "test.renamed")

	if Version("test.renamed") != 3 {
		t.Fatalf("version %d", Version("test.renamed"))
	}
	for v, in := range map[int]string{1: `{"text":"hi"}`, 2: `{"msg":"hi"}`, 3: `{"body":"hi"}`} {
		out, nv, err := Upgrade("test.renamed", v, json.RawMessage(in))
		if err != nil || nv != 3 || string(out) != `{"body":"hi"}` {
			t.Fatalf("v%d: %s v%d %v", v, out, nv, err)
		}
	}
	// Newer than this binary, or a type it does not know: passed through as stored.
	if out, nv, err := Upgrade("test.renamed", 4, json.RawMessage(`{"x":1}`)); err != nil || nv != 4 || string(out) != `{"x":1}` {
		t.Fatalf("newer: %s v%d %v", out, nv, err)
	}
	if out, nv, err := Upgrade("from.the.future", 1, json.RawMessage(`{}`)); err != nil || nv != 1 || string(out) != `{}` {
		t.Fatalf("unknown: %s v%d %v", out, nv, err)
	}
}

func TestEveryWrittenTypeIsVersioned(t *testing.T) {
	for typ := range registry {
		if Version(typ) < 1 {
			t.Fatalf("%s has no version", typ)
		}
	}
}
