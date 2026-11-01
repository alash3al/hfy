package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// Whatever a caller hands the recorder, the trace row lands: text that
// Postgres refuses is cleaned or replaced, never the reason the insert fails.
func TestRecorderStoresHostileRows(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	evil := "a\x00b\xff\xfe https://h.example/p?token=SECRET " + strings.Repeat("A", 200<<10)
	for _, c := range []struct {
		name    string
		rec     provider.Record
		request string // what the row's request must read back as; "" for none
	}{
		{"error", provider.Record{URL: "https://m.example/mcp\x00", Request: json.RawMessage(`{"a":1}`), Error: evil}, `{"a": 1}`},
		{"NUL in json", provider.Record{URL: "u", Request: json.RawMessage(`{"tool":"x\u0000"}`), Response: json.RawMessage(`{"k\u0000":1}`)}, `{"dropped": "not storable"}`},
		{"lone surrogate", provider.Record{URL: "u", Request: json.RawMessage(`{"t":"\ud800"}`)}, `{"dropped": "not storable"}`},
		{"too large", provider.Record{URL: "u", Request: json.RawMessage(`"` + strings.Repeat("x", maxTraceJSON) + `"`)}, `{"dropped": "not storable"}`},
		{"not json", provider.Record{URL: "u", Request: json.RawMessage(`{`)}, `{"dropped": "not storable"}`},
		{"nothing", provider.Record{URL: "u"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := f.rt.recorder(&f.env.TurnID, types.OutboundMCPRemote)(c.rec)
			if id == 0 {
				t.Fatal("trace row not written")
			}
			var url string
			var req, errText *string
			if err := f.rt.Store.Pool.QueryRow(ctx, "SELECT url, request::text, error FROM outbound_requests WHERE id = $1", id).
				Scan(&url, &req, &errText); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(url, "\x00") {
				t.Errorf("url: %q", url)
			}
			if c.request == "" && req != nil || c.request != "" && (req == nil || *req != c.request) {
				t.Errorf("request: %v, want %s", req, c.request)
			}
			if c.rec.Error == "" {
				return
			}
			if errText == nil || len(*errText) > types.MaxErrorBytes || !utf8.ValidString(*errText) ||
				strings.Contains(*errText, "SECRET") || !strings.Contains(*errText, "<url>") || !strings.HasPrefix(*errText, "ab") {
				t.Errorf("error text: %.80q (%d bytes)", *errText, len(*errText))
			}
		})
	}
}

// A request's tool schemas may nest far past the event-payload bound (maxDepth):
// the trace keeps them down to maxTraceDepth and drops anything deeper.
func TestTraceKeepsDeeperJSON(t *testing.T) {
	ctx := context.Background()
	f := newPersistFixture(t)
	for _, c := range []struct {
		depth   int
		dropped bool
	}{{maxDepth + 1, false}, {100, false}, {maxTraceDepth, false}, {maxTraceDepth + 1, true}} {
		id := f.rt.recorder(&f.env.TurnID, types.OutboundModel)(provider.Record{URL: "u",
			Request: json.RawMessage(nested(c.depth, true))})
		if id == 0 {
			t.Fatalf("depth %d: trace row not written", c.depth)
		}
		var req string
		if err := f.rt.Store.Pool.QueryRow(ctx, "SELECT request::text FROM outbound_requests WHERE id = $1", id).Scan(&req); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(req, "dropped"); got != c.dropped {
			t.Errorf("depth %d: dropped=%v, want %v", c.depth, got, c.dropped)
		}
	}
}
