package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/agent"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	os.Exit(m.Run())
}

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
		Server:    config.Server{Secret: "topsecret"},
		Queue:     config.Queue{Workers: 2, Lease: config.Duration(5 * time.Second), MaxAttempts: 2},
		Providers: map[string]config.Provider{"fake": {URL: fake.URL1(), Timeout: config.Duration(10 * time.Second)}},
		Agent: config.Agent{Model: "fake/m", System: "You help {{user}}.", Settings: config.Settings{ContextWindow: 100000},
			Vars: map[string]config.Var{"user": {Required: true, MaxLength: 10}, "tone": {Default: new("kind")}}},
	}
	if tweak != nil {
		tweak(cfg)
	}
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
	go rt.ListenCancel(ctx)
	hub := httpapi.NewHub(store)
	go hub.Run(ctx)
	srv := httptest.NewServer(httpapi.New(cfg, store, rt.Svc, hub).Handler())
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

func (a *api) newSession() string {
	a.t.Helper()
	code, out := a.do("POST", "/v1/sessions", "topsecret", nil)
	if code != 201 {
		a.t.Fatalf("create session: %d %v", code, out)
	}
	return out["id"].(string)
}

type sseEvent struct {
	ID, Type string
	Env      map[string]any
}

// stream opens the SSE endpoint and returns a channel of events.
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

func completed(ev sseEvent) bool {
	return ev.Type == "turn.state" && ev.Env["data"].(map[string]any)["state"] == "completed"
}

func TestServerSecretGuardsSessionCreation(t *testing.T) {
	a := start(t, nil)
	if code, _ := a.do("POST", "/v1/sessions", "", nil); code != 401 {
		t.Fatalf("no secret: %d", code)
	}
	if code, _ := a.do("POST", "/v1/sessions", "wrong", nil); code != 401 {
		t.Fatalf("wrong secret: %d", code)
	}
	a.newSession()
}

func TestMessageStreamResumeAndHistory(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "hello from the agent"}, fakeprov.Step{Content: "second answer"})
	sid := a.newSession()
	base := "/v1/sessions/" + sid

	// validation: required var, undeclared var, empty message
	if code, _ := a.do("POST", base+"/messages", "", map[string]any{"text": "hi"}); code != 400 {
		t.Fatalf("missing required var: %d", code)
	}
	if code, _ := a.do("POST", base+"/messages", "", map[string]any{"text": "hi", "vars": map[string]string{"user": "ann", "evil": "x"}}); code != 400 {
		t.Fatalf("undeclared var: %d", code)
	}
	if code, _ := a.do("POST", base+"/messages", "", map[string]any{"text": "hi", "vars": map[string]string{"user": "much too long name"}}); code != 400 {
		t.Fatalf("var too long: %d", code)
	}
	if code, _ := a.do("POST", base+"/messages", "", map[string]any{"text": " "}); code != 400 {
		t.Fatalf("empty message: %d", code)
	}
	if code, _ := a.do("POST", base+"/messages", "", map[string]any{"text": "x", "vars": map[string]string{"user": "a"},
		"attachments": []map[string]string{{"url": "https://cdn.example.com/a.png", "mime": "image/png"}}}); code != 400 {
		t.Fatalf("attachments without config: %d", code)
	}
	if code, _ := a.do("GET", "/v1/sessions/"+"not-a-uuid"+"/history", "", nil); code != 404 {
		t.Fatalf("bad id: %d", code)
	}

	events, closeStream := a.stream(base+"/events", nil)
	defer closeStream()
	code, out := a.do("POST", base+"/messages", "", map[string]any{"text": "hi there", "vars": map[string]string{"user": "ann"}})
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
	for _, leak := range []string{"scope", "session_id", "ephemeral", "request_id", `"id":1`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("leaked %q in %s", leak, raw)
		}
	}

	// history: durable only, grouped by turn, with a live cursor
	code, hist := a.do("GET", base+"/history", "", nil)
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
	resumed, closeResumed := a.stream(base+"/events", map[string]string{"Last-Event-ID": got[1].ID})
	defer closeResumed()
	code, _ = a.do("POST", base+"/messages", "", map[string]any{"text": "again", "vars": map[string]string{"user": "ann"}})
	if code != 202 {
		t.Fatal(code)
	}
	replay := waitFor(t, resumed, func(ev sseEvent) bool { return completed(ev) && ev.ID != got[len(got)-1].ID })
	if replay[0].ID != got[2].ID {
		t.Fatalf("replay must start right after Last-Event-ID, got %s %s", replay[0].Type, replay[0].ID)
	}
	// ?after= works for EventSource; "from now" skips history
	viaQuery, closeQ := a.stream(base+"/events?after="+got[len(got)-1].ID, nil)
	defer closeQ()
	first := waitFor(t, viaQuery, func(sseEvent) bool { return true })
	if first[0].Type != "turn" {
		t.Fatalf("?after= first event: %s", first[0].Type)
	}

	// pagination by turn
	code, page := a.do("GET", base+"/history?limit=1", "", nil)
	if code != 200 || len(page["turns"].([]any)) != 1 || page["next"] == nil {
		t.Fatalf("page 1: %d %v", code, page)
	}
	code, older := a.do("GET", base+"/history?limit=1&before="+page["next"].(string), "", nil)
	if code != 200 || older["turns"].([]any)[0].(map[string]any)["id"] != turn {
		t.Fatalf("page 2: %d %v", code, older)
	}
	if code, _ := a.do("GET", base+"/history?before=00000000-0000-4000-8000-000000000000", "", nil); code != 400 {
		t.Fatalf("unknown cursor: %d", code)
	}
	if code, _ := a.do("GET", base+"/events?after=00000000-0000-4000-8000-000000000000", "", nil); code != 400 {
		t.Fatalf("unknown after: %d", code)
	}
}

func TestWebhookAuth(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		switch body["key"] {
		case "alice-token":
			json.NewEncoder(w).Encode(map[string]any{"actor": "alice", "scope": "room-1", "vars": map[string]any{"user": "Alice", "tone": "formal"}})
		case "bob-token":
			json.NewEncoder(w).Encode(map[string]any{"actor": "bob"})
		case "expired":
			w.WriteHeader(http.StatusUnauthorized)
		case "garbage":
			w.Write([]byte("not json"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer hook.Close()
	a := start(t, func(c *config.Config) {
		c.Auth = &config.Auth{Webhook: hook.URL, Timeout: config.Duration(time.Second)}
	}, fakeprov.Step{Content: "hi alice"})
	sid := a.newSession()
	base := "/v1/sessions/" + sid

	for key, want := range map[string]int{"": 401, "expired": 401, "nope": 403, "garbage": 403} {
		if code, _ := a.do("GET", base+"/history", key, nil); code != want {
			t.Errorf("key %q: got %d want %d", key, code, want)
		}
	}
	hook.Close() // fail closed when the webhook is unreachable
	if code, _ := a.do("GET", base+"/history", "alice-token", nil); code != 403 {
		t.Fatalf("webhook down must deny, got %d", code)
	}
}

func TestWebhookIdentityVarsAndScope(t *testing.T) {
	var last map[string]string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&last)
		if last["key"] != "alice-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"actor": "alice", "scope": "room-1", "vars": map[string]any{"user": "Alice"}})
	}))
	defer hook.Close()
	a := start(t, func(c *config.Config) {
		c.Auth = &config.Auth{Webhook: hook.URL, Timeout: config.Duration(time.Second)}
	}, fakeprov.Step{Content: "hi alice"})
	sid := a.newSession()
	base := "/v1/sessions/" + sid

	// the key may also arrive as ?key= (EventSource) and reaches the webhook with the session id
	events, closeStream := a.stream(base+"/events?key=alice-token", nil)
	defer closeStream()
	if last["session_id"] != sid || last["key"] != "alice-token" {
		t.Fatalf("webhook request: %v", last)
	}
	// the webhook's var overrides the client's
	code, _ := a.do("POST", base+"/messages", "alice-token", map[string]any{"text": "hello", "vars": map[string]string{"user": "Mallory"}})
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
	rows, _ := a.rt.Store.Pool.Query(context.Background(), "SELECT DISTINCT scope FROM session_events")
	for rows.Next() {
		var s string
		rows.Scan(&s)
		scopes = append(scopes, s)
	}
	if len(scopes) != 1 || scopes[0] != "room-1" {
		t.Fatalf("scopes: %v", scopes)
	}
	// the key is not in the stored trace
	var n int
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM outbound_requests WHERE url = $1", hook.URL).Scan(&n)
	if n != 0 {
		t.Fatal("the auth webhook must never be written to outbound_requests")
	}
}

func TestApproveWithoutPendingIsConflictAndStopIsAccepted(t *testing.T) {
	a := start(t, nil)
	sid := a.newSession()
	base := "/v1/sessions/" + sid
	if code, _ := a.do("POST", base+"/approvals", "", map[string]any{"call_id": "x", "approve": true}); code != 409 {
		t.Fatalf("approve nothing: %d", code)
	}
	if code, _ := a.do("POST", base+"/approvals", "", map[string]any{"approve": true}); code != 400 {
		t.Fatalf("approve without call_id: %d", code)
	}
	if code, _ := a.do("POST", base+"/stop", "", nil); code != 202 {
		t.Fatalf("stop idle session: %d", code)
	}
	if code, _ := a.do("GET", "/v1/sessions/00000000-0000-4000-8000-000000000000/history", "", nil); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
}

// The auth webhook picks the agent per caller, here from the client's meta:
// each turn runs with that agent's prompt, model and vars, and its events are
// authored by that agent.
func TestWebhookPicksTheAgent(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key  string `json:"key"`
			Meta struct {
				Agent string `json:"agent"`
			} `json:"meta"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		switch in.Key {
		case "buyer": // buyers may ask for the store; the webhook decides
			json.NewEncoder(w).Encode(map[string]any{"actor": "u1", "agent": in.Meta.Agent, "vars": map[string]any{"shop": "Napoli"}})
		case "owner":
			json.NewEncoder(w).Encode(map[string]any{"actor": "u2", "vars": map[string]any{"user": "Sam"}}) // no agent: main
		default:
			json.NewEncoder(w).Encode(map[string]any{"actor": "u3", "agent": "nope"})
		}
	}))
	defer hook.Close()
	a := start(t, func(c *config.Config) {
		c.Auth = &config.Auth{Webhook: hook.URL, Timeout: config.Duration(time.Second)}
		c.Agents = map[string]config.Agent{"store": {Model: "fake/small", System: "You are {{shop}}'s store.",
			Settings: config.Settings{ContextWindow: 1000}, Vars: map[string]config.Var{"shop": {Required: true}}}}
	}, fakeprov.Step{Content: "store here"}, fakeprov.Step{Content: "main here"})
	sid := a.newSession()
	base := "/v1/sessions/" + sid
	events, closeStream := a.stream(base+"/events?key=buyer", nil)
	defer closeStream()

	asStore := map[string]any{"agent": "store"}
	if code, out := a.do("POST", base+"/messages", "buyer", map[string]any{"text": "pizza?", "meta": asStore}); code != 202 {
		t.Fatalf("buyer post: %d %v", code, out)
	}
	got := waitFor(t, events, completed)
	if code, out := a.do("POST", base+"/messages", "owner", map[string]any{"text": "status?"}); code != 202 {
		t.Fatalf("owner post: %d %v", code, out)
	}
	got = append(got, waitFor(t, events, completed)...)

	reqs := a.fake.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("model calls: %d", len(reqs))
	}
	for i, want := range []struct{ model, system string }{{"small", "You are Napoli's store."}, {"m", "You help Sam."}} {
		sys := reqs[i]["messages"].([]any)[0].(map[string]any)["content"]
		if reqs[i]["model"] != want.model || sys != want.system {
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
	if code, _ := a.do("POST", base+"/messages", "stranger", map[string]any{"text": "hi"}); code != http.StatusForbidden {
		t.Fatalf("an unknown agent must fail closed, got %d", code)
	}
	if code, _ := a.do("POST", base+"/messages", "owner", map[string]any{"text": "hi", "meta": nil}); code != 202 {
		t.Fatalf("a null meta is no meta, got %d", code)
	}
	for _, bad := range []any{"store", []int{1}, map[string]any{"x": strings.Repeat("a", 5000)}} {
		if code, _ := a.do("POST", base+"/messages", "buyer", map[string]any{"text": "hi", "meta": bad}); code != http.StatusBadRequest {
			t.Fatalf("meta %.20v must be rejected, got %d", bad, code)
		}
	}
}
