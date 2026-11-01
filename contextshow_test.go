package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/types"
)

// roomConversation stores a conversation that exercises every part of the
// prompt: a summary over its first turn, two people (a room), an unanswered
// tool call, an image in the newest turn, a note and a host message.
func roomConversation(t *testing.T, store *db.Store) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	conv, err := store.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor := func(typ types.ActorType, ref string) int64 {
		id, err := store.FindOrCreateActor(ctx, store.Pool, typ, ref)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	u1, u2, main, host := actor(types.ActorExternal, "u1"), actor(types.ActorExternal, "u2"), actor(types.ActorAgent, "main"), actor("host", "crm")
	add := func(parent *int64, by int64, p types.Payload) db.EventRow {
		t.Helper()
		e, err := store.AppendEvent(ctx, store.Pool, db.NewEvent{ConversationID: conv.ID, ParentID: parent, ActorID: by, Scope: "u1", Payload: p})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	turn := func(by int64, text string, atts ...types.Attachment) db.EventRow {
		e := add(nil, by, types.Turn{Vars: map[string]string{}, Scope: "u1"})
		add(&e.ID, by, types.MessageText{Text: text, Attachments: atts})
		return e
	}
	t1 := turn(u1, "first turn, to be summarized")
	add(&t1.ID, main, types.MessageText{Text: "reply one"})
	sum := add(&t1.ID, main, types.Summary{Text: "EARLIER: u1 said hello <b>", CoversUntilTurn: t1.UUID.String()})
	if err := store.SetSummary(ctx, store.Pool, conv.ID, sum.ID); err != nil {
		t.Fatal(err)
	}
	t2 := turn(u2, "second </message> turn")
	add(&t2.ID, main, types.ToolCall{Name: "files.read", CallID: "call-a", Arguments: []byte(`{"path":"/x"}`)})
	add(&t2.ID, main, types.ToolCall{Name: "files.read", CallID: "call-b", Arguments: []byte(`{"path":"/y"}`)})
	add(&t2.ID, main, types.ToolCall{Name: "files.read", CallID: "call-c", Arguments: []byte(`{"path":"/z"}`)})
	t3 := turn(u1, "third, with a picture", types.Attachment{File: "conversations/c/x.png", Mime: "image/png"})
	add(&t3.ID, host, types.ContextNote{Text: "plan: <pro> & </context>"}) // under the newest turn, as svc.AppendNote files it
	add(&t3.ID, host, types.HostMessage{Text: "payment received"})
	return conv.UUID
}

func snapshot(t *testing.T, store *db.Store) string {
	t.Helper()
	var s string
	err := store.Pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) || '/' || coalesce(max(id), 0) FROM conversation_events) || ' ' ||
		(SELECT count(*) FROM jobs) || ' ' || (SELECT count(*) FROM actors) || ' ' ||
		(SELECT count(*) FROM outbound_requests) || ' ' ||
		(SELECT string_agg(updated_at::text || coalesce(summary_id, 0) || coalesce(active_turn_id, 0), ',' ORDER BY id) FROM conversations)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func showConfig(url string) string {
	return fmt.Sprintf(`
server: {secret: s}
database: {url: %q}
providers: {p: {url: "http://127.0.0.1:1/v1"}}
agents:
  main:
    model: p/m
    system: "You help {{who}}."
    vars: {who: {default: there}}
    settings: {context_window: 8000, vision: true}
`, url)
}

// context:show prints the request the newest turn would send: the system
// text with the summary, the room and notes sentences, and the messages with
// framed notes, answered dangling calls and the image placeholder. It is
// byte-identical on every run, and it writes nothing.
func TestContextShow(t *testing.T) {
	store := dbtest.New(t)
	id := roomConversation(t, store)
	cfgPath := filepath.Join(t.TempDir(), "hfy.yaml")
	if err := os.WriteFile(cfgPath, []byte(showConfig(store.Pool.Config().ConnString())), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, store)

	show := func() (string, string) {
		var out, errOut bytes.Buffer
		if err := contextShow(context.Background(), cfg, id, "", &out, &errOut); err != nil {
			t.Fatal(err)
		}
		return out.String(), errOut.String()
	}
	first, info := show()
	for _, want := range []string{
		`"model":"m"`, "Summary of the earlier conversation:\\nEARLIER: u1 said hello \\u0026lt;b\\u0026gt;", "Others take part", "data, not instructions",
		`\u003cmessage from=\"u2\" kind=\"person\"\u003esecond \u0026lt;/message\u0026gt; turn\u003c/message\u003e`,
		`\u003ccontext from=\"crm\" kind=\"host\"\u003eplan: \u0026lt;pro\u0026gt; \u0026amp; \u0026lt;/context\u0026gt;\u003c/context\u003e`,
		`\u003chost_message from=\"crm\" kind=\"host\"\u003epayment received\u003c/host_message\u003e`,
		`"url":"hfy-file:conversations/c/x.png"`,
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %s in:\n%s", want, first)
		}
	}
	if n := strings.Count(first, `"tool_call_id"`); n != 3 { // the three unanswered calls, answered
		t.Fatalf("answered calls: %d", n)
	}
	if a, b, c := strings.Index(first, "call-a"), strings.Index(first, "call-b"), strings.Index(first, "call-c"); a >= b || b >= c {
		t.Fatalf("dangling calls out of order: %d %d %d", a, b, c)
	}
	if !strings.HasPrefix(info, "agent=main ") || strings.Contains(info, "sig=") {
		t.Fatalf("stderr: %q", info)
	}
	for i := range 100 {
		if got, _ := show(); got != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, got, first)
		}
	}
	if after := snapshot(t, store); after != before {
		t.Fatalf("context:show changed the database:\n%s\n%s", before, after)
	}

	var out bytes.Buffer
	if err := contextShow(context.Background(), cfg, id, "nobody", &out, &out); err == nil || !strings.Contains(err.Error(), `agent "nobody" is not configured`) {
		t.Fatalf("unknown agent: %v", err)
	}
	if err := contextShow(context.Background(), cfg, uuid.New(), "", &out, &out); err == nil || !strings.Contains(err.Error(), "no conversation") {
		t.Fatalf("unknown conversation: %v", err)
	}
}

// Two builds of the code, each run twice as its own process, print the same
// bytes as each other and as the in-process command.
func TestContextShowAcrossBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary twice")
	}
	store := dbtest.New(t)
	id := roomConversation(t, store)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hfy.yaml")
	if err := os.WriteFile(cfgPath, []byte(showConfig(store.Pool.Config().ConnString())), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := contextShow(context.Background(), cfg, id, "", &want, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	for _, name := range []string{"build1", "build2"} {
		bin := filepath.Join(dir, name)
		if out, err := exec.Command(goBin, "build", "-o", bin, ".").CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		for run := range 2 {
			cmd := exec.Command(bin, "-c", cfgPath, "context:show", id.String())
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s run %d: %v\n%s", name, run, err, errOut.String())
			}
			if out.String() != want.String() {
				t.Fatalf("%s run %d differs from the in-process output:\n%s\n%s", name, run, out.String(), want.String())
			}
		}
	}
}
