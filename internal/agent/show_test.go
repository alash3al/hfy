package agent_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// ShowPrompt is what the model gets: the body it builds for a conversation
// equals, message for message, the request the provider really received for
// the turn, plus what the turn added after it (the reply).
func TestShowPromptMatchesTheWire(t *testing.T) {
	e := setup(t, nil,
		fakeprov.Step{Content: "reply one"},
		fakeprov.Step{Content: "reply two"},
	)
	e.waitState(e.post("hello"), types.StateCompleted)
	if _, err := e.rt.Svc.AppendNote(e.ctx, svc.NoteInput{ConversationID: e.conv.ID, Actor: e.user,
		Type: types.EventContextNote, Text: "plan is pro"}); err != nil {
		t.Fatal(err)
	}
	e.waitState(e.post("and now?"), types.StateCompleted)

	wire := e.fake.Reqs()[1]
	v, err := e.rt.ShowPrompt(e.ctx, e.conv.UUID, "")
	if err != nil {
		t.Fatal(err)
	}
	var shown map[string]any
	if err := json.Unmarshal(v.Body, &shown); err != nil {
		t.Fatal(err)
	}
	wireMsgs, shownMsgs := wire["messages"].([]any), shown["messages"].([]any)
	if len(shownMsgs) != len(wireMsgs)+1 || !reflect.DeepEqual(shownMsgs[:len(wireMsgs)], wireMsgs) {
		t.Fatalf("the shown prompt differs from the request:\nwire  %v\nshown %v", wireMsgs, shownMsgs)
	}
	if last := shownMsgs[len(shownMsgs)-1].(map[string]any); last["role"] != "assistant" || last["content"] != "reply two" {
		t.Fatalf("the shown prompt ends with what the turn answered: %v", last)
	}
	for _, k := range []string{"model", "temperature", "top_p"} {
		if !reflect.DeepEqual(shown[k], wire[k]) {
			t.Errorf("%s: shown %v, sent %v", k, shown[k], wire[k])
		}
	}
	if v.Agent != "main" || v.EstimateTokens <= 0 || v.ContextWindow != 100000 {
		t.Fatalf("view: %+v", v)
	}
}
