package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/agent"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	httpapi.SetGrantCheckEvery(100 * time.Millisecond)
	os.Exit(m.Run())
}

const (
	secret = "topsecret"
	client = "/v1/conversation" // a client's routes: the grant names the conversation
)

type api struct {
	t    *testing.T
	url  string
	fake *fakeprov.Server
	cfg  *config.Config
	rt   *agent.Runtime
}

func start(t *testing.T, tweak func(*config.Config), script ...fakeprov.Step) *api {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fake := fakeprov.New(t, script...)
	cfg := &config.Config{
		Server:    config.Server{Secret: secret},
		Queue:     config.Queue{Workers: 2, Lease: config.Duration(5 * time.Second), MaxAttempts: 2},
		Providers: map[string]config.Provider{"fake": {URL: fake.URL1(), Timeout: config.Duration(10 * time.Second)}},
		Agent: config.Agent{Model: "fake/m", System: "You help {{user}}.", Settings: config.Settings{ContextWindow: 100000},
			Vars: map[string]config.Var{"user": {Default: new("friend")}, "tone": {Default: new("kind")}}},
	}
	if tweak != nil {
		tweak(cfg)
	}
	cfg.SetDefaults()
	store := dbtest.New(t, nil)
	t.Cleanup(cancel)
	rt, err := agent.NewRuntime(cfg, store, mcp.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	w := jobs.NewWorker(store, cfg.Queue.Lease.D(), []string{types.QueueTurns, types.QueueEmbeddings, types.QueueMaintenance})
	rt.RegisterJobs(w)
	go w.Run(ctx, 2)
	hub := httpapi.NewHub(store)
	go hub.Run(ctx)
	go store.Listen(ctx, func(channel, payload string) { // as harness.go does
		switch channel {
		case db.ChanEvents:
			hub.Notify(ctx, payload)
		case db.ChanJobs:
			w.Wake()
		case db.ChanCancel:
			rt.CancelTurn(payload)
		}
	}, w.Wake, db.ChanEvents, db.ChanJobs, db.ChanCancel)
	srv := httptest.NewUnstartedServer(httpapi.New(cfg, store, rt.Svc, hub).Handler())
	srv.Config.ReadTimeout = 300 * time.Millisecond // as in production (harness.go), only shorter: streams must outlive it
	srv.Start()
	t.Cleanup(srv.Close)
	time.Sleep(200 * time.Millisecond)
	return &api{t: t, url: srv.URL, fake: fake, cfg: cfg, rt: rt}
}

func (a *api) do(method, path, bearer string, body any) (int, map[string]any) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.url+path, rd)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (a *api) newConversation() string {
	a.t.Helper()
	code, out := a.do("POST", "/v1/conversations", secret, nil)
	if code != 201 {
		a.t.Fatalf("create conversation: %d %v", code, out)
	}
	return out["id"].(string)
}

// grant asks for a grant as the app's backend does (actor "u1" unless the
// body says otherwise) and returns its token and id.
func (a *api) grant(conv string, body map[string]any) (token, id string) {
	a.t.Helper()
	body = maps.Clone(body)
	if body == nil {
		body = map[string]any{}
	}
	body["conversation"] = conv
	if body["actor"] == nil {
		body["actor"] = "u1"
	}
	code, out := a.do("POST", "/v1/grants", secret, body)
	if code != 201 {
		a.t.Fatalf("create grant: %d %v", code, out)
	}
	return out["token"].(string), out["id"].(string)
}

// newToken is the common start: a conversation and one grant to it.
func (a *api) newToken() (token string) {
	a.t.Helper()
	token, _ = a.grant(a.newConversation(), nil)
	return token
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

type sseEvent struct {
	ID, Type string
	Env      map[string]any
}

// stream opens the SSE endpoint and returns a channel of events, closed when
// the server ends the stream.
func (a *api) stream(path string, header map[string]string) (<-chan sseEvent, func()) {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.url+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the reader goroutine closes it
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		a.t.Fatalf("stream status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		a.t.Fatalf("content type %q", ct)
	}
	ch := make(chan sseEvent, 1000)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.Type = line[7:]
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(line[6:]), &ev.Env)
			case line == "":
				if ev.ID != "" {
					ch <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	return ch, cancel
}

func waitFor(t *testing.T, ch <-chan sseEvent, stop func(sseEvent) bool) []sseEvent {
	t.Helper()
	var got []sseEvent
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed early after %d events", len(got))
			}
			got = append(got, ev)
			if stop(ev) {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out after %d events", len(got))
		}
	}
}

// waitClosed waits for the server to end a stream.
func waitClosed(t *testing.T, ch <-chan sseEvent, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("the stream is still open after %s", within)
		}
	}
}

func completed(ev sseEvent) bool {
	return ev.Type == "turn.state" && ev.Env["data"].(map[string]any)["state"] == "completed"
}

func TestServerSecretGuardsTheBackendRoutes(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/conversations", nil},
		{"POST", "/v1/grants", map[string]any{"conversation": conv, "actor": "u1"}},
		{"DELETE", "/v1/grants/00000000-0000-4000-8000-000000000000", nil},
	} {
		for _, key := range []string{"", "wrong"} {
			if code, _ := a.do(r.method, r.path, key, r.body); code != 401 {
				t.Errorf("%s %s with secret %q: %d", r.method, r.path, key, code)
			}
		}
	}
	// a grant's token is not the secret
	token, _ := a.grant(conv, nil)
	if code, _ := a.do("POST", "/v1/conversations", token, nil); code != 401 {
		t.Fatalf("a token must not create conversations: %d", code)
	}
}

func TestMessageStreamResumeAndHistory(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "hello from the agent"}, fakeprov.Step{Content: "second answer"})
	token := a.newToken()

	// validation
	if code, _ := a.do("POST", client+"/messages", token, map[string]any{"text": " "}); code != 400 {
		t.Fatalf("empty message: %d", code)
	}
	if code, _ := a.do("POST", client+"/messages", token, map[string]any{"text": "x",
		"attachments": []map[string]string{{"url": "https://cdn.example.com/a.png", "mime": "image/png"}}}); code != 400 {
		t.Fatalf("attachments without config: %d", code)
	}

	events, closeStream := a.stream(client+"/events", bearer(token))
	defer closeStream()
	code, out := a.do("POST", client+"/messages", token, map[string]any{"text": "hi there"})
	if code != 202 {
		t.Fatalf("post: %d %v", code, out)
	}
	turn := out["turn"].(string)
	got := waitFor(t, events, completed)

	// envelope shape and event vocabulary
	var types []string
	var deltas string
	for _, ev := range got {
		types = append(types, ev.Type)
		if ev.Env["id"] != ev.ID || ev.Env["type"] != ev.Type || ev.Env["actor"] == nil || ev.Env["created_at"] == nil {
			t.Fatalf("bad envelope: %v", ev.Env)
		}
		if ev.Type == "turn" && (ev.Env["parent"] != nil || ev.ID != turn) {
			t.Fatalf("turn event: %v", ev.Env)
		}
		if ev.Type != "turn" && ev.Env["parent"] != turn {
			t.Fatalf("%s must be a child of the turn: %v", ev.Type, ev.Env)
		}
		if ev.Type == "text.delta" {
			deltas += ev.Env["data"].(map[string]any)["text"].(string)
		}
	}
	want := []string{"turn", "message.text", "turn.state", "turn.state", "text.delta", "message.text", "model.usage", "turn.state"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events: %v", types)
	}
	if deltas != "hello from the agent" {
		t.Fatalf("deltas: %q", deltas)
	}
	// internal fields never leak
	raw, _ := json.Marshal(got[1].Env)
	for _, leak := range []string{"scope", "conversation_id", "ephemeral", "request_id", `"id":1`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("leaked %q in %s", leak, raw)
		}
	}

	// history: durable only, grouped by turn, with a live cursor
	code, hist := a.do("GET", client+"/history", token, nil)
	if code != 200 {
		t.Fatalf("history: %d", code)
	}
	turns := hist["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("history turns: %v", turns)
	}
	evs := turns[0].(map[string]any)["events"].([]any)
	for _, e := range evs {
		if e.(map[string]any)["type"] == "text.delta" {
			t.Fatal("history must not contain deltas")
		}
	}
	if len(evs) != 7 || hist["live_after"] != got[len(got)-1].ID {
		t.Fatalf("history events=%d live_after=%v want %v", len(evs), hist["live_after"], got[len(got)-1].ID)
	}

	// resume: Last-Event-ID replays only what came after it, then continues live
	resumed, closeResumed := a.stream(client+"/events", map[string]string{"Authorization": "Bearer " + token, "Last-Event-ID": got[1].ID})
	defer closeResumed()
	code, _ = a.do("POST", client+"/messages", token, map[string]any{"text": "again"})
	if code != 202 {
		t.Fatal(code)
	}
	replay := waitFor(t, resumed, func(ev sseEvent) bool { return completed(ev) && ev.ID != got[len(got)-1].ID })
	if replay[0].ID != got[2].ID {
		t.Fatalf("replay must start right after Last-Event-ID, got %s %s", replay[0].Type, replay[0].ID)
	}
	// EventSource: the token and ?after= in the query; "from now" skips history
	viaQuery, closeQ := a.stream(client+"/events?token="+token+"&after="+got[len(got)-1].ID, nil)
	defer closeQ()
	first := waitFor(t, viaQuery, func(sseEvent) bool { return true })
	if first[0].Type != "turn" {
		t.Fatalf("?after= first event: %s", first[0].Type)
	}

	// pagination by turn
	code, page := a.do("GET", client+"/history?limit=1", token, nil)
	if code != 200 || len(page["turns"].([]any)) != 1 || page["next"] == nil {
		t.Fatalf("page 1: %d %v", code, page)
	}
	code, older := a.do("GET", client+"/history?limit=1&before="+page["next"].(string), token, nil)
	if code != 200 || older["turns"].([]any)[0].(map[string]any)["id"] != turn {
		t.Fatalf("page 2: %d %v", code, older)
	}
	if code, _ := a.do("GET", client+"/history?before=00000000-0000-4000-8000-000000000000", token, nil); code != 400 {
		t.Fatalf("unknown cursor: %d", code)
	}
	if code, _ := a.do("GET", client+"/events?after=00000000-0000-4000-8000-000000000000", token, nil); code != 400 {
		t.Fatalf("unknown after: %d", code)
	}
}

// Creating a grant checks everything the backend may get wrong, so a mistake
// is the backend's 400 now and never a client's failed turn later.
func TestGrantValidation(t *testing.T) {
	a := start(t, func(c *config.Config) {
		c.Agents = map[string]config.Agent{"store": {Model: "fake/small", System: "You are {{shop}}'s store.",
			Settings: config.Settings{ContextWindow: 1000}, Vars: map[string]config.Var{"shop": {Required: true}}}}
	})
	conv := a.newConversation()
	ok := map[string]any{"conversation": conv, "actor": "u1"}
	with := func(k string, v any) map[string]any {
		b := maps.Clone(ok)
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
		return b
	}
	for name, c := range map[string]struct {
		body map[string]any
		want int
	}{
		"no conversation":      {with("conversation", nil), 400},
		"unknown conversation": {with("conversation", "00000000-0000-4000-8000-000000000000"), 404},
		"no actor":             {with("actor", nil), 400},
		"blank actor":          {with("actor", "  "), 400},
		"long actor":           {with("actor", strings.Repeat("a", 257)), 400},
		"long scope":           {with("scope", strings.Repeat("a", 257)), 400},
		"unknown agent":        {with("agent", "nope"), 400},
		"undeclared var":       {with("vars", map[string]string{"usr": "x"}), 400},
		"missing required var": {with("agent", "store"), 400},
		"expires_in 0":         {with("expires_in", 0), 400},
		"expires_in over 24h":  {with("expires_in", 86401), 400},
		"not json":             {nil, 400},
	} {
		var body any = c.body
		if c.body == nil {
			body = "not an object"
		}
		if code, out := a.do("POST", "/v1/grants", secret, body); code != c.want {
			t.Errorf("%s: got %d %v, want %d", name, code, out, c.want)
		}
	}

	code, out := a.do("POST", "/v1/grants", secret, ok)
	if code != 201 || !strings.HasPrefix(out["token"].(string), "hfy_") || len(out["token"].(string)) != 47 || out["id"] == nil {
		t.Fatalf("grant: %d %v", code, out)
	}
	exp, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	if err != nil || time.Until(exp) < 59*time.Minute || time.Until(exp) > 61*time.Minute {
		t.Fatalf("default expiry is an hour: %v %v", out["expires_at"], err)
	}
	// the token is shown once: the database holds only its hash
	var n int
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM grants g WHERE g::text LIKE '%' || $1 || '%'",
		strings.TrimPrefix(out["token"].(string), "hfy_")).Scan(&n)
	if n != 0 {
		t.Fatal("the token is stored")
	}
}

// A token works until its grant expires or is revoked, then gets the same
// 401 as a token that never existed. ?token= is for EventSource only.
func TestTokens(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	token, id := a.grant(conv, nil)

	for name, tok := range map[string]string{"none": "", "garbage": "hfy_nope", "the secret": secret} {
		if code, _ := a.do("GET", client+"/history", tok, nil); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := a.do("GET", client+"/history", token, nil); code != 200 {
		t.Fatalf("valid token: %d", code)
	}
	if code, _ := a.do("GET", client+"/history?token="+token, "", nil); code != 401 {
		t.Fatalf("?token= outside the stream: %d", code)
	}

	if code, _ := a.do("DELETE", "/v1/grants/"+id, secret, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := a.do("DELETE", "/v1/grants/"+id, secret, nil); code != 204 {
		t.Fatalf("revoking twice: %d", code)
	}
	if code, out := a.do("GET", client+"/history", token, nil); code != 401 || out["error"] != "invalid or expired token" {
		t.Fatalf("revoked token: %d %v", code, out)
	}
	for _, bad := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if code, _ := a.do("DELETE", "/v1/grants/"+bad, secret, nil); code != 404 {
			t.Fatalf("revoke %s: %d", bad, code)
		}
	}

	short, _ := a.grant(conv, map[string]any{"expires_in": 1})
	if code, _ := a.do("GET", client+"/history", short, nil); code != 200 {
		t.Fatalf("fresh short grant: %d", code)
	}
	time.Sleep(1100 * time.Millisecond)
	if code, out := a.do("GET", client+"/history", short, nil); code != 401 || out["error"] != "invalid or expired token" {
		t.Fatalf("expired token: %d %v", code, out)
	}
}

// An open stream ends with its grant: on expiry, and soon after a revocation.
func TestStreamsEndWithTheirGrant(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()

	token, id := a.grant(conv, nil)
	revoked, closeRevoked := a.stream(client+"/events", bearer(token))
	defer closeRevoked()
	time.Sleep(300 * time.Millisecond) // several re-checks while the grant holds
	select {
	case _, ok := <-revoked:
		if !ok {
			t.Fatal("a live grant's stream ended")
		}
	default:
	}
	if code, _ := a.do("DELETE", "/v1/grants/"+id, secret, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	waitClosed(t, revoked, 2*time.Second)

	short, _ := a.grant(conv, map[string]any{"expires_in": 1})
	expiring, closeExpiring := a.stream(client+"/events", bearer(short))
	defer closeExpiring()
	waitClosed(t, expiring, 3*time.Second)
}

// The grant carries the identity: actor, scope and vars reach the turn; a
// client's own vars are ignored.
func TestGrantIdentityVarsAndScope(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "hi alice"})
	token, _ := a.grant(a.newConversation(), map[string]any{"actor": "alice", "scope": "room-1", "vars": map[string]string{"user": "Alice"}})

	events, closeStream := a.stream(client+"/events?token="+token, nil)
	defer closeStream()
	code, _ := a.do("POST", client+"/messages", token, map[string]any{"text": "hello", "vars": map[string]string{"user": "Mallory"}})
	if code != 202 {
		t.Fatalf("post: %d", code)
	}
	got := waitFor(t, events, completed)
	sys := a.fake.Reqs()[0]["messages"].([]any)[0].(map[string]any)["content"]
	if sys != "You help Alice." {
		t.Fatalf("system prompt: %v", sys)
	}
	// the actor is recorded on the user's message
	var actor map[string]any
	for _, ev := range got {
		if ev.Type == "message.text" && ev.Env["actor"].(map[string]any)["type"] == "external" {
			actor = ev.Env["actor"].(map[string]any)
		}
	}
	if actor == nil || actor["ref"] != "alice" {
		t.Fatalf("actor: %v", actor)
	}
	// the scope is stored on the turn and copied to its events
	var scopes []string
	rows, _ := a.rt.Store.Pool.Query(context.Background(), "SELECT DISTINCT scope FROM conversation_events")
	for rows.Next() {
		var s string
		rows.Scan(&s)
		scopes = append(scopes, s)
	}
	if len(scopes) != 1 || scopes[0] != "room-1" {
		t.Fatalf("scopes: %v", scopes)
	}
}

// A conversation is a room: everyone granted to it sees every turn, whoever
// started it, and their scope defaults to their actor.
func TestGrantsShareARoom(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "hello both"})
	conv := a.newConversation()
	alice, _ := a.grant(conv, map[string]any{"actor": "alice"})
	bob, _ := a.grant(conv, map[string]any{"actor": "bob"})
	toBob, closeBob := a.stream(client+"/events", bearer(bob))
	defer closeBob()
	if code, _ := a.do("POST", client+"/messages", alice, map[string]any{"text": "hi"}); code != 202 {
		t.Fatalf("alice posts: %d", code)
	}
	got := waitFor(t, toBob, completed)
	if got[0].Type != "turn" || got[0].Env["actor"].(map[string]any)["ref"] != "alice" {
		t.Fatalf("bob sees alice's turn: %v", got[0].Env)
	}
	var scope string
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT scope FROM conversation_events WHERE type = 'turn'").Scan(&scope)
	if scope != "alice" {
		t.Fatalf("scope defaults to the actor: %q", scope)
	}
}

func TestApproveWithoutPendingIsConflictAndStopIsAccepted(t *testing.T) {
	a := start(t, nil)
	token := a.newToken()
	if code, _ := a.do("POST", client+"/approvals", token, map[string]any{"call_id": "x", "approve": true}); code != 409 {
		t.Fatalf("approve nothing: %d", code)
	}
	if code, _ := a.do("POST", client+"/approvals", token, map[string]any{"approve": true}); code != 400 {
		t.Fatalf("approve without call_id: %d", code)
	}
	if code, _ := a.do("POST", client+"/stop", token, nil); code != 202 {
		t.Fatalf("stop idle conversation: %d", code)
	}
}

// The grant picks the agent: each turn runs with that agent's prompt, model
// and vars, and its events are authored by that agent.
func TestGrantPicksTheAgent(t *testing.T) {
	a := start(t, func(c *config.Config) {
		c.Agents = map[string]config.Agent{"store": {Model: "fake/small", System: "You are {{shop}}'s store.",
			Settings: config.Settings{ContextWindow: 1000}, Vars: map[string]config.Var{"shop": {Required: true}}}}
	}, fakeprov.Step{Content: "store here"}, fakeprov.Step{Content: "main here"})
	conv := a.newConversation()
	buyer, buyerID := a.grant(conv, map[string]any{"actor": "u1", "agent": "store", "vars": map[string]string{"shop": "Napoli"}})
	owner, ownerID := a.grant(conv, map[string]any{"actor": "u2", "vars": map[string]string{"user": "Sam"}}) // no agent: main
	events, closeStream := a.stream(client+"/events", bearer(buyer))
	defer closeStream()

	if code, out := a.do("POST", client+"/messages", buyer, map[string]any{"text": "pizza?"}); code != 202 {
		t.Fatalf("buyer post: %d %v", code, out)
	}
	got := waitFor(t, events, completed)
	if code, out := a.do("POST", client+"/messages", owner, map[string]any{"text": "status?"}); code != 202 {
		t.Fatalf("owner post: %d %v", code, out)
	}
	got = append(got, waitFor(t, events, completed)...)

	reqs := a.fake.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("model calls: %d", len(reqs))
	}
	for i, want := range []struct{ model, system string }{{"small", "You are Napoli's store."}, {"m", "You help Sam."}} {
		sys := reqs[i]["messages"].([]any)[0].(map[string]any)["content"].(string)
		if reqs[i]["model"] != want.model || !strings.HasPrefix(sys, want.system) { // turn 1 follows the store's reply: a room
			t.Fatalf("turn %d ran as model=%v system=%v", i, reqs[i]["model"], sys)
		}
	}
	replies := map[string]string{} // reply text -> its agent
	storeTurns := 0
	for _, ev := range got {
		actor := ev.Env["actor"].(map[string]any)
		switch {
		case ev.Type == "message.text" && actor["type"] == "agent":
			replies[ev.Env["data"].(map[string]any)["text"].(string)] = actor["ref"].(string)
		case ev.Type == "turn" && ev.Env["data"].(map[string]any)["agent"] == "store" && actor["ref"] == "u1":
			storeTurns++
		}
	}
	if replies["store here"] != "store" || replies["main here"] != "main" || storeTurns != 1 {
		t.Fatalf("reply authors: %v, store turns recorded: %d", replies, storeTurns)
	}

	// the config changed after the grants were made: an agent that is gone
	// fails closed, a var that became required is a server error
	ctx := context.Background()
	a.rt.Store.Pool.Exec(ctx, "UPDATE grants SET agent = 'gone' WHERE uuid = $1", ownerID)
	if code, _ := a.do("POST", client+"/messages", owner, map[string]any{"text": "hi"}); code != http.StatusForbidden {
		t.Fatalf("an unknown agent must fail closed, got %d", code)
	}
	a.rt.Store.Pool.Exec(ctx, "UPDATE grants SET vars = '{}' WHERE uuid = $1", buyerID)
	if code, _ := a.do("POST", client+"/messages", buyer, map[string]any{"text": "hi"}); code != http.StatusInternalServerError {
		t.Fatalf("a required var the grant lacks is a server error, got %d", code)
	}
}

// An SSE stream outlives the server's ReadTimeout, which bounds only how long
// a request may take to arrive.
func TestStreamOutlivesReadTimeout(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "late but here"})
	token := a.newToken()
	events, closeStream := a.stream(client+"/events", bearer(token))
	defer closeStream()
	time.Sleep(1500 * time.Millisecond) // five ReadTimeouts
	if code, out := a.do("POST", client+"/messages", token, map[string]any{"text": "still there?"}); code != 202 {
		t.Fatalf("post: %d %v", code, out)
	}
	waitFor(t, events, completed)
}

// Every route bounds its body, not only messages.
func TestEveryBodyIsBounded(t *testing.T) {
	a := start(t, nil)
	token := a.newToken()
	huge := map[string]any{"call_id": "x", "result": strings.Repeat("a", 3<<20)}
	if code, _ := a.do("POST", client+"/approvals", token, huge); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 3 MiB approval body must be refused, got %d", code)
	}
	if code, _ := a.do("POST", "/v1/grants", secret, map[string]any{"actor": strings.Repeat("a", 3<<20)}); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 3 MiB grant body must be refused, got %d", code)
	}
}

// Bodies are JSON whatever the Content-Type says: `curl -d` (a form type, as
// in the README's quick start) and a missing type both work.
func TestBodiesAreJSONWhateverTheContentType(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	post := func(path, auth, contentType, body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, a.url+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+auth)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := post("/v1/grants", secret, "application/x-www-form-urlencoded", `{"conversation":"`+conv+`","actor":"u1"}`)
	if code != 201 {
		t.Fatalf("grant with a form content type: %d %v", code, out)
	}
	token := out["token"].(string)
	if code, out := post(client+"/messages", token, "", `{"text":"hello"}`); code != 202 {
		t.Fatalf("message without a content type: %d %v", code, out)
	}
	if code, out := post(client+"/messages", token, "application/json", `{"text":`); code != 400 || out["error"] != "invalid json body" {
		t.Fatalf("broken json: %d %v", code, out)
	}
}

// Browsers on the configured origins may call the client routes, errors
// included (so a page can read its 401). The backend routes never answer a
// browser, and an origin not listed gets no CORS headers.
func TestCORS(t *testing.T) {
	const site, evil = "https://shop.example.com", "https://evil.example.com"
	a := start(t, func(c *config.Config) { c.Server.CORSOrigins = []string{site} })
	token := a.newToken()
	auth := "Bearer " + token

	r := a.browser(http.MethodOptions, client+"/messages", site, "")
	if r.code != http.StatusNoContent || r.Get("Access-Control-Allow-Origin") != site ||
		!strings.Contains(r.Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(strings.ToLower(r.Get("Access-Control-Allow-Headers")), "authorization") ||
		r.Get("Access-Control-Max-Age") != "3600" || r.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", r.code, r.Header)
	}
	for _, c := range []struct {
		path, auth string
		code       int
	}{
		{client + "/history", auth, http.StatusOK},
		{client + "/history", "", http.StatusUnauthorized}, // a page must be able to read its 401
		{client + "/events?token=" + token, "", http.StatusOK},
	} {
		if r := a.browser(http.MethodGet, c.path, site, c.auth); r.code != c.code || r.Get("Access-Control-Allow-Origin") != site {
			t.Errorf("GET %s: %d %v", c.path, r.code, r.Header)
		}
	}
	for _, c := range []struct{ method, path, origin, auth string }{
		{http.MethodOptions, client + "/messages", evil, ""},
		{http.MethodGet, client + "/history", evil, auth},
		{http.MethodOptions, "/v1/grants", site, ""},
		{http.MethodOptions, "/v1/conversations", site, ""},
		{http.MethodPost, "/v1/conversations", site, "Bearer " + secret},
	} {
		if v := a.browser(c.method, c.path, c.origin, c.auth).Get("Access-Control-Allow-Origin"); v != "" {
			t.Errorf("%s %s answered a browser from %s: %q", c.method, c.path, c.origin, v)
		}
	}
}

// Without cors_origins no route answers a browser on another origin.
func TestNoCORSByDefault(t *testing.T) {
	a := start(t, nil)
	if v := a.browser(http.MethodOptions, client+"/messages", "https://shop.example.com", "").Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("CORS without config: %q", v)
	}
}

type answer struct {
	code int
	http.Header
}

// browser sends what a page on origin would: an OPTIONS is a preflight for a
// POST with a token and JSON. It returns the status and headers only.
func (a *api) browser(method, path, origin, auth string) answer {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a stream answers, then is cut
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, a.url+path, nil)
	req.Header.Set("Origin", origin)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	resp.Body.Close()
	return answer{resp.StatusCode, resp.Header}
}
