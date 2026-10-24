package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
)

const askForAPI = `{"questions":[{"id":"size","text":"Which size?","type":"single","options":[{"value":"m"},{"value":"l"}]},
	{"id":"extras","text":"Extras?","type":"multi","options":[{"value":"gift"},{"value":"wrap"}]},
	{"id":"note","text":"Note?","type":"text","required":false,"placeholder":"optional"}]}`

func waitPending(t *testing.T, events <-chan sseEvent, callID string) {
	t.Helper()
	waitFor(t, events, func(ev sseEvent) bool {
		return ev.Type == "approval.requested" && ev.Env["data"].(map[string]any)["call_id"] == callID
	})
}

// The answers field is validated against the paused call's stored questions,
// and the API names any mistake: 400s, 403 for a non-owner.
func TestApproveValidatesTheAnswers(t *testing.T) {
	cases := []struct {
		name    string
		answers any
		want    int
	}{
		{"unknown id", map[string]any{"size": "m", "extras": []string{"gift"}, "colour": "red"}, 400},
		{"bad option", map[string]any{"size": "xxl", "extras": []string{"gift"}}, 400},
		{"wrong type for option", map[string]any{"size": 1, "extras": []string{"gift"}}, 400},
		{"multi subset", map[string]any{"size": "m", "extras": []string{"gift", "tax"}}, 400},
		{"multi element type", map[string]any{"size": "m", "extras": []any{7}}, 400},
		{"multi must be array", map[string]any{"size": "m", "extras": "gift"}, 400},
		{"required multi empty", map[string]any{"size": "m", "extras": []any{}}, 400},
		{"text too long", map[string]any{"size": "m", "extras": []string{"gift"}, "note": strings.Repeat("a", 2001)}, 400},
		{"note not a string", map[string]any{"size": "m", "extras": []string{"gift"}, "note": 3}, 400},
		{"missing required", map[string]any{"size": "m"}, 400},
		{"answers not an object", map[string]any{"size": "m", "extras": []string{"gift"}, "note": []string{"m"}}, 400},
		{"null answers", nil, 400},
		// last: the only approving row, so the script stays aligned (an
		// approval consumes an extra scripted step for the model's next move)
		{"valid answers", map[string]any{"size": "m", "extras": []string{"gift"}}, 202},
	}
	ask := fakeprov.Step{Calls: []fakeprov.Call{{ID: "q", Name: "hfy__ask_user", Args: askForAPI}}}
	script := make([]fakeprov.Step, len(cases)+2) // asks; the owner's turn below takes the last one
	for i := range script {
		script[i] = ask
	}
	a := start(t, func(c *config.Config) { c.Agent.AskUser = true }, script...)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := a.newToken()
			events, closeEV := a.stream(client+"/events", bearer(token))
			defer closeEV()
			if code, _ := a.do("POST", client+"/messages", token, map[string]any{"text": "fit me"}); code != 202 {
				t.Fatalf("post: %d", code)
			}
			waitPending(t, events, "q")
			code, out := a.do("POST", client+"/approvals", token,
				map[string]any{"call_id": "q", "approve": true, "answers": c.answers})
			if code != c.want {
				t.Fatalf("want %d, got %d %v", c.want, code, out)
			}
		})
	}

	// answers are optional-skipped and empty answers accepted: covered by the
	// agent tests; here the 202 row above proves the wire accepts a good one.

	// Only who started the turn answers its ask: 403 for everyone else.
	conv := a.newConversation()
	u1, _ := a.grant(conv, map[string]any{"actor": "u1"})
	u2, _ := a.grant(conv, map[string]any{"actor": "u2"})
	events, closeEV := a.stream(client+"/events", bearer(u1))
	defer closeEV()
	if code, _ := a.do("POST", client+"/messages", u1, map[string]any{"text": "fit me"}); code != 202 {
		t.Fatalf("u1 post: %d", code)
	}
	waitPending(t, events, "q")
	if code, _ := a.do("POST", client+"/approvals", u2,
		map[string]any{"call_id": "q", "approve": true, "answers": map[string]any{"size": "m", "extras": []string{"gift"}}}); code != 403 {
		t.Fatalf("u2 answered u1's ask: http %s", http.StatusText(code))
	}
	code, out := a.do("POST", client+"/approvals", u1,
		map[string]any{"call_id": "q", "approve": true, "answers": map[string]any{"size": "m", "extras": []string{"wrap"}}})
	if code != 202 {
		t.Fatalf("u1 answered own ask: %d %v", code, out)
	}
}
