package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
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
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/fakemcp"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeRun()
	slog.SetLogLoggerLevel(slog.LevelError)
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	ctx   context.Context
	store *db.Store
	rt    *agent.Runtime
	fake  *fakeprov.Server
	sess  db.SessionRow
	user  int64
}

// setup starts a runtime and workers over a fresh database and a fake provider.
func setup(t *testing.T, tweak func(*config.Config), script ...fakeprov.Step) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fake := fakeprov.New(t, script...)
	cfg := &config.Config{
		Server:    config.Server{Secret: "s"},
		Queue:     config.Queue{Workers: 2, Lease: config.Duration(5 * time.Second), MaxAttempts: 2},
		Providers: map[string]config.Provider{"fake": {URL: fake.URL1(), Timeout: config.Duration(10 * time.Second)}},
		MCP: map[string]config.MCPServer{"files": {Type: "local", Command: []string{os.Args[0]},
			Environment: map[string]string{"HFY_FAKE_MCP": "1"}}},
		Agent: config.Agent{Model: "fake/m", System: "You help {{who}}.",
			Settings: config.Settings{ContextWindow: 100000},
			Vars:     map[string]config.Var{"who": {Default: ptr("everyone")}}},
	}
	if tweak != nil {
		tweak(cfg)
	}
	var emb *db.Embeddings
	if cfg.Embeddings != nil {
		emb = &db.Embeddings{Model: "e", Dims: cfg.Embeddings.Dimensions}
	}
	store := dbtest.New(t, emb)
	t.Cleanup(cancel) // runs before the database is dropped
	mgr := mcp.NewManager()
	mcp.StartAll(ctx, mgr, cfg.MCP)
	rt, err := agent.NewRuntime(cfg, store, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	w := jobs.NewWorker(store, cfg.Queue.Lease.D(), []string{types.QueueTurns, types.QueueEmbeddings, types.QueueMaintenance})
	rt.RegisterJobs(w)
	if cfg.Queue.Workers > 0 { // tests that drive RunTurn by hand set Workers to 0
		go w.Run(ctx, cfg.Queue.Workers)
	}
	go rt.ListenCancel(ctx)
	time.Sleep(200 * time.Millisecond) // let the listeners attach

	sess, err := store.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := store.FindOrCreateActor(ctx, types.ActorAnonymous, types.NoScope)
	return &env{t: t, ctx: ctx, store: store, rt: rt, fake: fake, sess: sess, user: user}
}

func ptr[T any](v T) *T { return &v }

func (e *env) post(text string) (turnID int64) {
	e.t.Helper()
	vars, err := svc.ResolveVars(e.rt.Cfg.Agent.Vars, nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	u, err := e.rt.Svc.Post(e.ctx, svc.PostInput{SessionID: e.sess.ID, Actor: e.user, Scope: types.NoScope,
		Vars: vars, Text: text})
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := e.store.TurnIDByUUID(e.ctx, e.sess.ID, u)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// events returns the durable events of a turn.
func (e *env) events(turn int64) []db.EventRow {
	ev, err := e.store.TurnEvents(e.ctx, e.store.Pool, turn)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) state(turn int64) types.TurnState {
	st, _ := e.store.TurnStateOf(e.ctx, e.store.Pool, turn)
	if st == nil {
		return ""
	}
	return st.State
}

func (e *env) waitState(turn int64, want types.TurnState) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e.state(turn) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("turn state is %q, want %q\n%s", e.state(turn), want, e.dump(turn))
}

func (e *env) dump(turn int64) string {
	var b strings.Builder
	for _, ev := range e.events(turn) {
		b.WriteString(string(ev.Type) + " " + ev.ActorRef + " " + string(ev.Data) + "\n")
	}
	return b.String()
}

// find returns the payload JSON of events of a type.
func (e *env) find(turn int64, typ types.EventType) []map[string]any {
	var out []map[string]any
	for _, ev := range e.events(turn) {
		if ev.Type == typ {
			var m map[string]any
			json.Unmarshal(ev.Data, &m)
			out = append(out, m)
		}
	}
	return out
}

func TestSimpleReply(t *testing.T) {
	e := setup(t, nil, fakeprov.Step{Content: "hello there", Prompt: 12, Output: 3})
	turn := e.post("hi")
	e.waitState(turn, types.StateCompleted)

	msgs := e.find(turn, types.EventMessageText)
	if len(msgs) != 2 || msgs[1]["text"] != "hello there" {
		t.Fatalf("messages: %v\n%s", msgs, e.dump(turn))
	}
	if u := e.find(turn, types.EventModelUsage); len(u) != 1 || u[0]["input"] != float64(12) {
		t.Fatalf("usage: %v", u)
	}
	last := e.find(turn, types.EventTurnState)
	if usage := last[len(last)-1]["usage"].(map[string]any); usage["output"] != float64(3) {
		t.Fatalf("final state usage: %v", last[len(last)-1])
	}
	// The vars default was rendered into the system prompt, and the trace exists.
	req := e.fake.Reqs()[0]["messages"].([]any)[0].(map[string]any)["content"]
	if req != "You help everyone." {
		t.Fatalf("system prompt: %v", req)
	}
	var n int
	e.store.Pool.QueryRow(e.ctx, "SELECT count(*) FROM outbound_requests WHERE kind='model'").Scan(&n)
	if n != 1 {
		t.Fatalf("outbound_requests: %d", n)
	}
	s, _ := e.store.SessionByID(e.ctx, e.sess.ID)
	if s.InputTokens != 12 || s.ActiveTurnID != nil {
		t.Fatalf("session: %+v", s)
	}
}

func TestToolSearchThenCall(t *testing.T) {
	e := setup(t, nil,
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "c1", Name: "harness__tool_search", Args: `{"query":"echo"}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "c2", Name: "files__echo", Args: `{"text":"x"}`}}},
		fakeprov.Step{Content: "done"},
	)
	turn := e.post("use echo")
	e.waitState(turn, types.StateCompleted)

	results := e.find(turn, types.EventToolResult)
	if len(results) != 2 || results[1]["content"] != "echo: x" {
		t.Fatalf("results: %v", results)
	}
	reqs := e.fake.Reqs()
	if names := toolNames(reqs[0]); strings.Contains(names, "files__echo") || !strings.Contains(names, "harness__tool_search") {
		t.Fatalf("step 1 tools: %s", names)
	}
	if names := toolNames(reqs[1]); !strings.Contains(names, "files__echo") {
		t.Fatalf("tool_search must unlock echo, step 2 tools: %s", names)
	}
}

func toolNames(req map[string]any) string {
	var names []string
	tools, _ := req["tools"].([]any)
	for _, t := range tools {
		names = append(names, t.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return strings.Join(names, ",")
}

func TestLoopRules(t *testing.T) {
	same := fakeprov.Step{Calls: []fakeprov.Call{{ID: "c", Name: "files__echo", Args: `{"text":"again"}`}}}
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
		// unknown tool, bad args, then the same call over and over
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "u", Name: "nope__x", Args: `{}`}}},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "b", Name: "files__echo", Args: `{}`}}},
		same, same, same, same, same, same, same,
	)
	turn := e.post("go")
	e.waitState(turn, types.StateFailed)

	results := e.find(turn, types.EventToolResult)
	if !strings.Contains(results[0]["content"].(string), "unknown tool") || !strings.Contains(results[0]["content"].(string), "files__echo") {
		t.Fatalf("unknown tool result: %v", results[0])
	}
	if !strings.Contains(results[1]["content"].(string), "missing required field") {
		t.Fatalf("bad args result: %v", results[1])
	}
	if results[2]["content"] != "echo: again" || !strings.Contains(results[4]["content"].(string), "exact call") {
		t.Fatalf("repeat handling: %v", results)
	}
	states := e.find(turn, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "loop detected") {
		t.Fatalf("final reason: %v", r)
	}
}

func TestLimits(t *testing.T) {
	step := func(i string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: i, Name: "files__echo", Args: `{"text":"` + i + `"}`}}}
	}
	e := setup(t, func(c *config.Config) { c.Agent.Limits.Steps = 2 }, step("1"), step("2"), step("3"))
	turn := e.post("go")
	e.waitState(turn, types.StateFailed)
	states := e.find(turn, types.EventTurnState)
	if r := states[len(states)-1]["reason"].(string); !strings.Contains(r, "2 steps") {
		t.Fatalf("reason: %s", r)
	}
	if n := len(e.fake.Reqs()); n != 2 {
		t.Fatalf("model calls: %d", n)
	}
}

func TestModelFailureFailsTurn(t *testing.T) {
	e := setup(t, nil, fakeprov.Step{Status: 400})
	turn := e.post("hi")
	e.waitState(turn, types.StateFailed)
}

func TestApprovalApproveAndReject(t *testing.T) {
	call := func(id string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: id, Name: "files__write_file", Args: `{"text":"a.txt"}`}}}
	}
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Agent.Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
	}, call("w1"), fakeprov.Step{Content: "written"}, call("w2"), fakeprov.Step{Content: "ok, not written"})

	t1 := e.post("write it")
	e.waitState(t1, types.StateAwaitingApproval)
	if n := len(e.find(t1, types.EventToolResult)); n != 0 {
		t.Fatal("tool ran before approval")
	}
	s, _ := e.store.SessionByID(e.ctx, e.sess.ID)
	if s.ActiveTurnID == nil {
		t.Fatal("a paused turn keeps the session blocked")
	}
	other := e.post("queued behind")
	if e.state(other) != types.StateQueued {
		t.Fatalf("second turn must queue, got %s", e.state(other))
	}
	if err := e.rt.Svc.Approve(e.ctx, e.sess.ID, e.user, types.ApprovalDecided{CallID: "w1", Approve: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.sess.ID, e.user, types.ApprovalDecided{CallID: "w1", Approve: true}); err == nil {
		t.Fatal("second decision on the same call must be refused")
	}
	e.waitState(t1, types.StateCompleted)
	if r := e.find(t1, types.EventToolResult); len(r) != 1 || r[0]["content"] != "wrote a.txt" {
		t.Fatalf("approved call result: %v", r)
	}

	// the queued turn now runs and pauses on its own call; reject it with a result
	e.waitState(other, types.StateAwaitingApproval)
	if err := e.rt.Svc.Approve(e.ctx, e.sess.ID, e.user, types.ApprovalDecided{CallID: "w2", Approve: false, Result: "no, not today"}); err != nil {
		t.Fatal(err)
	}
	e.waitState(other, types.StateCompleted)
	if r := e.find(other, types.EventToolResult); len(r) != 1 || r[0]["content"] != "no, not today" {
		t.Fatalf("rejected call result: %v", r)
	}
}

func TestApprovalTimeoutRejects(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Agent.Approval = &config.Approval{Tools: []string{"files.*"}, Timeout: config.Duration(300 * time.Millisecond)}
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "w", Name: "files__echo", Args: `{"text":"x"}`}}}, fakeprov.Step{Content: "fine"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	d := e.find(turn, types.EventApprovalDecided)
	if len(d) != 1 || d[0]["reason"] != "timeout" || d[0]["approve"] != false {
		t.Fatalf("decided: %v", d)
	}
}

func TestStopRunningTurn(t *testing.T) {
	block := make(chan struct{})
	e := setup(t, nil, fakeprov.Step{Delay: block, Content: "never"})
	turn := e.post("slow one")
	e.waitState(turn, types.StateRunning)
	time.Sleep(300 * time.Millisecond) // the model call is in flight
	queued := e.post("behind it")
	if err := e.rt.Svc.Stop(e.ctx, e.sess.ID, e.user); err != nil {
		t.Fatal(err)
	}
	e.waitState(turn, types.StateStopped)
	if e.state(queued) != types.StateCancelled {
		t.Fatalf("queued turn: %s", e.state(queued))
	}
	if len(e.find(turn, types.EventTurnCancelRequested)) != 1 {
		t.Fatal("cancel request event missing")
	}
	s, _ := e.store.SessionByID(e.ctx, e.sess.ID)
	if s.ActiveTurnID != nil {
		t.Fatal("active turn must be cleared")
	}
}

func TestStopWhileAwaitingApproval(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Agent.Approval = &config.Approval{Tools: []string{"files.*"}, Timeout: config.Duration(time.Minute)}
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "w", Name: "files__echo", Args: `{"text":"x"}`}}})
	turn := e.post("go")
	e.waitState(turn, types.StateAwaitingApproval)
	if err := e.rt.Svc.Stop(e.ctx, e.sess.ID, e.user); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateStopped {
		t.Fatalf("state: %s", e.state(turn))
	}
}

func TestCrashRecoveryMarksInFlightCallInterrupted(t *testing.T) {
	// No workers: this test plays the part of the worker that runs again after a crash.
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Queue.Workers = 0
	}, fakeprov.Step{Content: "I will check first"})
	turn := e.post("go")
	if ok, err := e.rt.Svc.StartTurn(e.ctx, e.sess.ID, turn); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The first worker appended a tool call and died before writing its result.
	main, _ := e.store.FindOrCreateActor(e.ctx, types.ActorAgent, "main")
	row, _ := e.store.EventByID(e.ctx, e.store.Pool, turn)
	e.store.AppendEvent(e.ctx, e.store.Pool, db.NewEvent{SessionID: e.sess.ID, ParentID: &turn, ActorID: main, Scope: row.Scope,
		Payload: types.ToolCall{Name: "files.write_file", CallID: "lost", Arguments: json.RawMessage(`{"text":"x"}`)}})

	// The lease expired and the job runs again: attempt 2.
	if err := e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.find(turn, types.EventToolCallInterrupted)); n != 1 {
		t.Fatalf("interrupted markers: %d\n%s", n, e.dump(turn))
	}
	if n := len(e.find(turn, types.EventToolResult)); n != 0 {
		t.Fatal("an in-flight call must never be re-executed")
	}
	body, _ := json.Marshal(e.fake.Reqs()[0]["messages"])
	if !strings.Contains(string(body), "interrupted") {
		t.Fatalf("model not told about the interruption: %s", body)
	}
	if e.state(turn) != types.StateCompleted {
		t.Fatalf("state: %s", e.state(turn))
	}
}

func TestApprovedCallIsNotReExecutedAfterACrash(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Queue.Workers = 0
		c.Agent.Approval = &config.Approval{Tools: []string{"files.*"}, Timeout: config.Duration(time.Minute)}
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "w", Name: "files__write_file", Args: `{"text":"a"}`}}}, fakeprov.Step{Content: "done"})
	turn := e.post("go")
	ctx := e.ctx
	if err := e.rt.RunTurn(ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateAwaitingApproval {
		t.Fatalf("state: %s", e.state(turn))
	}
	if err := e.rt.Svc.Approve(ctx, e.sess.ID, e.user, types.ApprovalDecided{CallID: "w", Approve: true}); err != nil {
		t.Fatal(err)
	}
	// The resumed job crashed after the decision: on its second attempt the
	// approved call may or may not have run, so it must not run again.
	if err := e.rt.RunTurn(ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.find(turn, types.EventToolResult)); n != 0 {
		t.Fatalf("approved call re-executed after a crash\n%s", e.dump(turn))
	}
	if n := len(e.find(turn, types.EventToolCallInterrupted)); n != 1 {
		t.Fatalf("want one interrupted marker\n%s", e.dump(turn))
	}
}

func TestImageAttachmentsOnlyInTheirOwnTurn(t *testing.T) {
	png := []byte("\x89PNG fake image bytes")
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}))
	defer files.Close()
	host := strings.TrimPrefix(files.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]

	e := setup(t, func(c *config.Config) {
		c.Agent.Settings.Vision = true
		c.Attachments = &config.Attachments{AllowedHosts: []string{host}, Types: []string{"image/png"}, MaxBytes: 1 << 20}
	}, fakeprov.Step{Content: "nice picture"}, fakeprov.Step{Content: "still here"})

	post := func(text string, atts ...types.Attachment) int64 {
		vars, _ := svc.ResolveVars(e.rt.Cfg.Agent.Vars, nil, nil)
		u, err := e.rt.Svc.Post(e.ctx, svc.PostInput{SessionID: e.sess.ID, Actor: e.user, Scope: types.NoScope, Vars: vars, Text: text, Attachments: atts})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := e.store.TurnIDByUUID(e.ctx, e.sess.ID, u)
		return id
	}
	t1 := post("look", types.Attachment{URL: files.URL + "/a.png", Mime: "image/png"})
	e.waitState(t1, types.StateCompleted)
	// A local provider cannot fetch URLs, so the harness inlines the bytes.
	user := e.fake.Reqs()[0]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	img := user[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") || user[0].(map[string]any)["text"] != "look" {
		t.Fatalf("first turn content: %v", user)
	}
	// The stored trace does not keep the inlined bytes.
	var leaked int
	e.store.Pool.QueryRow(e.ctx, `SELECT count(*) FROM outbound_requests WHERE request::text LIKE '%base64,%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("inline image bytes were stored in outbound_requests")
	}

	t2 := post("and now?")
	e.waitState(t2, types.StateCompleted)
	msgs := e.fake.Reqs()[1]["messages"].([]any)
	first := msgs[1].(map[string]any)["content"]
	if s, ok := first.(string); !ok || !strings.Contains(s, "[image: image/png]") {
		t.Fatalf("later turns get a placeholder, got %v", first)
	}
}

// A stop that commits while the model is answering (its NOTIFY not yet
// delivered) must still win over an approval pause that follows.
func TestStopBeforeApprovalPauseStopsTheTurn(t *testing.T) {
	block := make(chan struct{})
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		c.Queue.Workers = 0
		c.Agent.Approval = &config.Approval{Tools: []string{"files.*"}, Timeout: config.Duration(time.Minute)}
	}, fakeprov.Step{Delay: block, Calls: []fakeprov.Call{{ID: "w", Name: "files__write_file", Args: `{"text":"a"}`}}})
	turn := e.post("go")
	done := make(chan error, 1)
	go func() { done <- e.rt.RunTurn(e.ctx, svc.RunTurnPayload{TurnEventID: turn}, db.JobRow{Attempts: 1}) }()
	e.waitState(turn, types.StateRunning)
	time.Sleep(200 * time.Millisecond) // the model call is in flight

	// What Stop commits, minus the notification, which has not arrived yet.
	row, _ := e.store.EventByID(e.ctx, e.store.Pool, turn)
	e.store.AppendEvent(e.ctx, e.store.Pool, db.NewEvent{SessionID: e.sess.ID, ParentID: &turn, ActorID: e.user,
		Scope: row.Scope, Payload: types.TurnCancelRequested{}})
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.state(turn) != types.StateStopped {
		t.Fatalf("state: %s\n%s", e.state(turn), e.dump(turn))
	}
	s, _ := e.store.SessionByID(e.ctx, e.sess.ID)
	if s.ActiveTurnID != nil {
		t.Fatal("active turn must be cleared")
	}
}
