package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestExpand(t *testing.T) {
	e := env(map[string]string{"A": "x", "EMPTY": ""})
	cases := map[string]string{
		"${A}":          "x",
		"a-${A}-b":      "a-x-b",
		"${MISSING:-d}": "d",
		"${EMPTY:-d}":   "d",
		"${EMPTY}":      "",
		"$$HOME":        "$HOME",
		"cost $5":       "cost $5",
		"${A:-unused}":  "x",
		"trailing $":    "trailing $",
		"${MISSING:-}":  "",
	}
	for in, want := range cases {
		got, err := Expand(in, e)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Expand("${NOPE}", e); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("missing var must name the variable, got %v", err)
	}
}

func TestExampleParses(t *testing.T) {
	c, err := Parse([]byte(ExampleYAML), env(map[string]string{
		"HFY_SERVER_SECRET": "s3cret", "DATABASE_URL": "postgres://x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.Workers != 4 || c.Retention.Trace.D() != 30*24*time.Hour || c.Server.Listen != ":8080" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestEnvOnlyInValuesAndNumbers(t *testing.T) {
	y := `
server: { secret: s }
database: { url: "${U}" }
queue: { workers: "${W}" }
providers: { p: { url: http://x } }
agent: { model: p/m, settings: { context_window: 10 } }
`
	c, err := Parse([]byte(y), env(map[string]string{"U": "pg://", "W": "7"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.Workers != 7 || c.Database.URL != "pg://" {
		t.Fatalf("got %+v", c)
	}
}

func TestRender(t *testing.T) {
	got := Render("Hi {{ name }} at {{corp}}, {{unknown}}", map[string]string{"name": "Bob", "corp": "Acme"})
	if got != "Hi Bob at Acme, {{unknown}}" {
		t.Fatal(got)
	}
	// substituted values are never re-expanded
	if got := Render("{{a}}", map[string]string{"a": "{{b}}", "b": "x"}); got != "{{b}}" {
		t.Fatal(got)
	}
}

func TestValidationErrors(t *testing.T) {
	base := `
server: { secret: s }
database: { url: u }
providers: { p: { url: http://x } }
`
	cases := map[string]string{
		"agent: { model: nope/m, settings: { context_window: 10 } }": `unknown provider "nope"`,
		"agent: { model: p/m }": "context_window",
		"agent: { model: p/m, settings: { context_window: 1 }, system: 'hi {{who}}' }":                                "{{who}}",
		"agent: { model: p/m, settings: { context_window: 1 }, mcp: [ghost] }":                                        `unknown server "ghost"`,
		"agent: { model: p/m, settings: { context_window: 1 } }\nsubagents: { main: { description: d, model: p/m } }": "reserved",
		"agent: { model: p/m, settings: { context_window: 1 } }\nsubagents: { a: { model: p/m } }":                    "description",
		"agent: { model: p/m, settings: { context_window: 1 } }\nsubagents: { now: { description: d, model: p/m } }":  "taken by the built-in tool hfy.now",
		"agent: { model: p/m, settings: { context_window: 1 } }\nauth: { webhook: http://hook }":                      `unknown key "auth"`,
		"agent: { model: p/m, settings: { context_window: 1 } }\nmcp: { hfy: { type: local, command: [x] } }":         "reserved for hfy's built-in tools",
	}
	for tail, want := range cases {
		_, err := Parse([]byte(base+tail), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
	if _, err := Parse([]byte("database: {url: u}\nagent: {model: p/m}"), env(nil)); err == nil || !strings.Contains(err.Error(), "server.secret") {
		t.Errorf("missing secret must fail, got %v", err)
	}
}

func TestUnits(t *testing.T) {
	if d, _ := ParseDuration("30d"); d != 30*24*time.Hour {
		t.Error(d)
	}
	for range 200 { // map-order bugs show up only some of the time
		for in, want := range map[string]int64{"5MB": 5 << 20, "512KB": 512 << 10, "1GB": 1 << 30, "7B": 7, "42": 42, "5 mb": 5 << 20} {
			if got, err := ParseBytes(in); err != nil || got != want {
				t.Fatalf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
			}
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: postgres://x}\nproviders: {p: {url: http://x}}\n"
	for _, c := range []struct{ yaml, want string }{
		{"agent:\n  model: p/m\n  settings: {context_window: 10}\n  aproval: {tools: [\"files.*\"]}\n", `line 7: unknown key "aproval" in config.agent`},
		{"agent: {model: p/m, settings: {context_window: 10, temprature: 0.2}}\n", `unknown key "temprature" in config.agent.settings`},
		{"mcp: {files: {type: local, comand: [x]}}\nagent: {model: p/m, settings: {context_window: 10}}\n", `unknown key "comand" in config.mcp.files`},
		{"agent: {model: p/m, settings: {context_window: 10}}\nretension: {trace: 1d}\n", `unknown key "retension" in config`},
	} {
		_, err := Parse([]byte(base+c.yaml), func(string) (string, bool) { return "", false })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("want %q, got %v", c.want, err)
		}
	}
}

func TestNamedAgents(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: postgres://x}\nproviders: {p: {url: http://x}}\n" +
		"agent: {model: p/m, settings: {context_window: 10}}\n"
	cfg, err := Parse([]byte(base+"agents:\n  store: {model: p/small, system: 'Store {{name}}', vars: {name: {required: true}}, settings: {context_window: 5}, approval: {tools: [\"cart.*\"]}}\n"),
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := cfg.AgentNamed("store"); !ok || a.Model != "p/small" || a.Approval.Timeout.D() == 0 {
		t.Fatalf("store agent: %+v", a)
	}
	if a, ok := cfg.AgentNamed(""); !ok || a.Model != "p/m" {
		t.Fatal("empty name must be the main agent")
	}
	for yaml, want := range map[string]string{
		"agents: {summarizer: {model: p/m, settings: {context_window: 1}}}\n":                                      "agents.summarizer: name is reserved",
		"agents: {x: {model: q/m, settings: {context_window: 1}}}\n":                                               `agents.x.model: unknown provider "q"`,
		"agents: {x: {model: p/m}}\n":                                                                              "agents.x.settings.context_window is required",
		"agents: {x: {model: p/m, settings: {context_window: 1}, system: '{{nope}}'}}\n":                           "agents.x.system uses {{nope}}",
		"agents: {x: {model: p/m, settings: {context_window: 1}}}\nsubagents: {x: {description: d, model: p/m}}\n": "subagents.x: name is reserved or taken",
	} {
		if _, err := Parse([]byte(base+yaml), func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
}

// An origin is accepted only as a browser writes it, so it can ever match.
func TestCORSOrigins(t *testing.T) {
	base := "database: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	for origins, ok := range map[string]bool{
		`["https://shop.example.com"]`:                       true,
		`["http://localhost:5173", "https://a.example.com"]`: true,
		`["*"]`:                             true,
		`["https://shop.example.com/"]`:     false, // trailing slash
		`["https://shop.example.com/app"]`:  false,
		`["https://shop.example.com:443"]`:  false, // browsers omit the default port
		`["http://shop.example.com:80"]`:    false,
		`["https://Shop.example.com"]`:      false,
		`["shop.example.com"]`:              false,
		`["ftp://shop.example.com"]`:        false,
		`["https://shop.example.com?x=1"]`:  false,
		`["*", "https://shop.example.com"]`: false, // "*" alone
	} {
		_, err := Parse([]byte(base+"server: {secret: s, cors_origins: "+origins+"}\n"), env(nil))
		if (err == nil) != ok {
			t.Errorf("%s: ok=%v, got %v", origins, ok, err)
		}
	}
}

func TestEmbedValidation(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	if _, err := Parse([]byte(base+"embed: {accent: red}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.accent") {
		t.Errorf("bad accent must fail naming embed.accent, got %v", err)
	}
	c, err := Parse([]byte(base), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Embed.Dir != "auto" || c.Embed.Theme != "auto" || c.Embed.Accent != "#6366f1" || c.Embed.Density != "comfortable" || c.Embed.Layout != "bubbles" || c.Embed.ToolCalls != "summary" {
		t.Errorf("embed defaults: %+v", c.Embed)
	}
}

func TestMockProviderParses(t *testing.T) {
	y := `
server: { secret: s }
database: { url: u }
providers:
  p: { url: http://x }
  fake: { type: mock, mock: { reply: "Hello from mock" } }
agent: { model: p/m, settings: { context_window: 10 } }
`
	c, err := Parse([]byte(y), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers["p"].Type != "openai" {
		t.Errorf("provider without type must default to openai, got %q", c.Providers["p"].Type)
	}
	if got := c.Providers["fake"]; got.Type != "mock" || got.Mock.Reply != "Hello from mock" {
		t.Errorf("mock provider: %+v", got)
	}
}

func TestMockProviderBadErrorStatus(t *testing.T) {
	y := `
server: { secret: s }
database: { url: u }
providers:
  fake: { type: mock, mock: { error_status: 200 } }
agent: { model: fake/m, settings: { context_window: 10 } }
`
	_, err := Parse([]byte(y), env(nil))
	if err == nil || !strings.Contains(err.Error(), "providers.fake.mock.error_status") {
		t.Errorf("bad error_status must fail naming providers.fake.mock.error_status, got %v", err)
	}
}

func TestRemovedEmbeddingsKeyFailsWithTailoredError(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	if _, err := Parse([]byte(base), env(nil)); err != nil {
		t.Fatalf("the base config must parse: %v", err)
	}
	const want = "embeddings was removed in v2; expose memory as an MCP tool"
	for name, set := range map[string]string{
		"map":       "embeddings: {model: p/e, dimensions: 8}\n",
		"empty map": "embeddings: {}\n",
		"null":      "embeddings: null\n",
		"bare":      "embeddings:\n",
		"scalar":    "embeddings: yes\n",
		"list":      "embeddings: [a]\n",
		"unset env": "embeddings: ${NOPE}\n", // refused before ${VAR} expansion could fail first
	} {
		_, err := Parse([]byte(base+set), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "line 5") {
			t.Errorf("%s: want %q at line 5, got %v", name, want, err)
		}
	}
	// Keys match case-sensitively everywhere: a variant is just unknown.
	_, err := Parse([]byte(base+"Embeddings: {}\n"), env(nil))
	if err == nil || !strings.Contains(err.Error(), `unknown key "Embeddings"`) {
		t.Errorf("case variant: want the generic unknown-key error, got %v", err)
	}
	// Only the top-level key is removed; a nested one is not special.
	_, err = Parse([]byte(base+"embed: {embeddings: 1}\n"), env(nil))
	if err == nil || strings.Contains(err.Error(), "removed in v2") {
		t.Errorf("nested key: want the generic unknown-key error, got %v", err)
	}
}

func TestFilesystemDefaults(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	c, err := Parse([]byte(base+"filesystem: {dsn: file:///tmp/hfy-files, types: [image/png]}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Filesystem; f.MaxBytes != 10<<20 || f.UploadTTL.D() != 5*time.Minute || f.DownloadTTL.D() != 15*time.Minute {
		t.Errorf("filesystem defaults: %+v", f)
	}
	for yaml, want := range map[string]string{
		"filesystem: {dsn: ftp://bucket, types: [image/png]}\n":                            "filesystem.dsn",
		"filesystem: {dsn: file:///tmp/hfy-files}\n":                                       "filesystem.types", // no attachments to inherit from
		"filesystem: {dsn: \"s3://bucket\", types: [image/png]}\n":                         "region=<region>",
		"filesystem: {dsn: \"s3://bucket?endpoint=http://s3:9000\", types: [image/png]}\n": "region=<region>",
	} {
		if _, err := Parse([]byte(base+yaml), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
	if _, err := Parse([]byte(base+"filesystem: {dsn: \"s3://bucket?region=eu-west-1\", types: [image/png]}\n"), env(nil)); err != nil {
		t.Errorf("s3 DSN with region: %v", err)
	}
}

// Server-owned approval policy: patterns are validated like agent ones, and
// a pattern naming only a core tool can never work (core tools never wait).
func TestMCPServerApproval(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	y := base + "mcp: {files: {type: remote, url: http://x, approval: [\"[bad\"], approve_destructive: true}}\n"
	if _, err := Parse([]byte(y), env(nil)); err == nil || !strings.Contains(err.Error(), `mcp.files.approval: bad pattern "[bad"`) {
		t.Errorf("bad pattern must fail naming mcp.<name>.approval, got %v", err)
	}
	c, err := Parse([]byte(base+`mcp: {files: {type: remote, url: http://x, approval: [refund, delete_*]}}`+"\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a := c.MCP["files"].Approval; len(a) != 2 || a[0] != "refund" {
		t.Fatalf("approval parsed: %v", a)
	}
	plain := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\n"
	for _, p := range []string{"hfy.now", "hfy.math_calc", "hfy.tool_search"} {
		y := plain + `agent: {model: p/m, settings: {context_window: 1}, approval: {tools: ["` + p + `"]}}` + "\n"
		if _, err := Parse([]byte(y), env(nil)); err == nil || !strings.Contains(err.Error(), "core tool and never needs approval") {
			t.Errorf("%q must be rejected, got %v", p, err)
		}
	}
	if _, err := Parse([]byte(plain+"agent: {model: p/m, settings: {context_window: 1}, approval: {tools: [\"hfy.*\"]}}\n"), env(nil)); err != nil {
		t.Errorf("a wide hfy.* pattern is fine (it guards sub-agents): %v", err)
	}
}

func TestApprovalRequires(t *testing.T) {
	a := &Approval{Tools: []string{"files.write", "cart.*"}}
	for _, c := range []struct {
		a    *Approval
		tool string
		want bool
	}{
		{nil, "files.write", false}, // no policy: nothing waits
		{a, "files.write", true},
		{a, "cart.checkout", true},
		{a, "files.read", false},
	} {
		if got := c.a.Requires(c.tool); got != c.want {
			t.Errorf("%v.Requires(%q) = %v, want %v", c.a, c.tool, got, c.want)
		}
	}
}

// The ui node opts one MCP server into cards and questions; hosts are checked
// and unknown keys inside it rejected.
func TestMCPServerUI(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
	c, err := Parse([]byte(base+`mcp: {files: {type: remote, url: http://x, ui: {image_hosts: [cdn.example.com, "*.s3.amazonaws.com"]}}}`+"\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	u := c.MCP["files"].UI
	if u == nil || len(u.ImageHosts) != 2 || u.ImageHosts[0] != "cdn.example.com" {
		t.Fatalf("ui parsed: %+v", u)
	}
	for yaml, want := range map[string]string{
		`mcp: {files: {type: remote, url: http://x, ui: {image_host: [cdn.example.com]}}}`:       `unknown key "image_host" in config.mcp.files.ui`,
		`mcp: {files: {type: remote, url: http://x, ui: {image_hosts: ["https://evil.com/x"]}}}`: "mcp.files.ui.image_hosts",
		`mcp: {files: {type: remote, url: http://x, ui: {image_hosts: ["not a host"]}}}`:         "mcp.files.ui.image_hosts",
		`mcp: {files: {type: remote, url: http://x, ui: {image_hosts: [""]}}}`:                   "mcp.files.ui.image_hosts",
	} {
		if _, err := Parse([]byte(base+yaml+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

// Model request controls parse as written, and each bad value stops startup
// with an error naming the agent and the key.
func TestModelRequestControls(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\n"
	c, err := Parse([]byte(base+"agent: {model: p/m, settings: {context_window: 1, reasoning_effort: none, max_output_tokens: 512, call_timeout: 30s, extra: {cache_control: {type: ephemeral}, reasoning: {effort: high}}}}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Agent.Settings
	if s.ReasoningEffort != "none" || s.MaxOutputTokens != 512 || s.CallTimeout.D() != 30*time.Second ||
		s.Extra["reasoning"].(map[string]any)["effort"] != "high" || c.Providers["p"].MaxTokensParam != "max_tokens" {
		t.Fatalf("parsed: %+v", s)
	}
	c, err = Parse([]byte("server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x, max_tokens_param: max_completion_tokens}}\n"+
		"agent: {model: p/m, settings: {context_window: 1}}\n"), env(nil))
	if err != nil || c.Providers["p"].MaxTokensParam != "max_completion_tokens" {
		t.Fatalf("max_tokens_param: %v %+v", err, c.Providers["p"])
	}

	bad := map[string]string{
		"agent: {model: p/m, settings: {context_window: 1, reasoning_effort: maybe}}":                                 "agent.settings.reasoning_effort must be one of none, minimal, low, medium, high, xhigh, max",
		"agents: {x: {model: p/m, settings: {context_window: 1, reasoning_effort: Low}}}":                             "agents.x.settings.reasoning_effort",
		"subagents: {r: {description: d, model: p/m, settings: {reasoning_effort: x}}}":                               "subagents.r.settings.reasoning_effort",
		"summarizer: {model: p/m, settings: {context_window: 1, reasoning_effort: x}}":                                "summarizer.settings.reasoning_effort",
		"agent: {model: p/m, settings: {context_window: 1, max_output_tokens: -1}}":                                   "agent.settings.max_output_tokens must be greater than 0",
		"agents: {x: {model: p/m, settings: {context_window: 1, max_output_tokens: -1}}}":                             "agents.x.settings.max_output_tokens",
		"summarizer: {model: p/m, settings: {context_window: 1, max_output_tokens: -2}}":                              "summarizer.settings.max_output_tokens",
		"agent: {model: p/m, settings: {context_window: 1, call_timeout: -5s}}":                                       "agent.settings.call_timeout must not be negative",
		"subagents: {r: {description: d, model: p/m, settings: {call_timeout: -1s}}}":                                 "subagents.r.settings.call_timeout",
		"agent: {model: p/m, settings: {context_window: 1, reasoning_efort: low}}":                                    `unknown key "reasoning_efort" in config.agent.settings`,
		"agent: {model: p/m, settings: {context_window: 1, extra: {model: x}}}":                                       "agent.settings.extra.model is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {messages: []}}}":                                   "agent.settings.extra.messages is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {tools: []}}}":                                      "agent.settings.extra.tools is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {stream: false}}}":                                  "agent.settings.extra.stream is set by hfy",
		"agents: {x: {model: p/m, settings: {context_window: 1, extra: {stream_options: {}}}}}":                       "agents.x.settings.extra.stream_options is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {n: 2}}}":                                           "agent.settings.extra.n is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {Tools: []}}}":                                      "agent.settings.extra.Tools is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {MODEL: x}}}":                                       "agent.settings.extra.MODEL is set by hfy",
		"agents: {x: {model: p/m, settings: {context_window: 1, extra: {Stream: true}}}}":                             "agents.x.settings.extra.Stream is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, extra: {Foo: 1, foo: 2}}}":                                 "agent.settings.extra: Foo and foo differ only in case",
		"subagents: {r: {description: d, model: p/m, settings: {extra: {model: x}}}}":                                 "subagents.r.settings.extra.model is set by hfy",
		"summarizer: {model: p/m, settings: {context_window: 1, extra: {messages: []}}}":                              "summarizer.settings.extra.messages is set by hfy",
		"agent: {model: p/m, settings: {context_window: 1, reasoning_effort: low, extra: {reasoning_effort: high}}}":  "agent.settings.extra.reasoning_effort clashes with settings.reasoning_effort",
		"agent: {model: p/m, settings: {context_window: 1, max_output_tokens: 9, extra: {max_completion_tokens: 5}}}": "agent.settings.extra.max_completion_tokens clashes with settings.max_output_tokens",
		"agent: {model: p/m, settings: {context_window: 1, max_output_tokens: 9, extra: {max_tokens: 5}}}":            "agent.settings.extra.max_tokens clashes with settings.max_output_tokens",
		"agent: {model: p/m, settings: {context_window: 1, max_output_tokens: 9, extra: {MAX_TOKENS: 5}}}":            "agent.settings.extra.MAX_TOKENS clashes with settings.max_output_tokens",
		"agent: {model: p/m, settings: {context_window: 1, reasoning_effort: low, extra: {Reasoning_Effort: high}}}":  "agent.settings.extra.Reasoning_Effort clashes with settings.reasoning_effort",
		"agent: {model: p/m, settings: {context_window: 1, temperature: 0.2, extra: {temperature: 0.5}}}":             "agent.settings.extra.temperature clashes with settings.temperature",
		"agents: {x: {model: p/m, settings: {context_window: 1, top_p: 0.9, extra: {top_p: 0.5}}}}":                   "agents.x.settings.extra.top_p clashes with settings.top_p",
		"agent: {model: p/m, settings: {context_window: 1, extra: {bad: .inf}}}":                                      "agent.settings.extra: values must be JSON-serialisable",
		"summarizer: {model: p/m, settings: {context_window: 1, extra: {bad: .nan}}}":                                 "summarizer.settings.extra: values must be JSON-serialisable",
	}
	for tail, want := range bad {
		_, err := Parse([]byte(base+tail+"\n"), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", tail, want, err)
		}
	}
	// the agent's own context window is still required next to the controls
	if _, err := Parse([]byte(base+"agent: {model: p/m, settings: {reasoning_effort: low}}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "context_window") {
		t.Errorf("controls do not replace context_window: %v", err)
	}
	if _, err := Parse([]byte("server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x, max_tokens_param: tokens}}\n"+
		"agent: {model: p/m, settings: {context_window: 1}}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "providers.p.max_tokens_param must be") {
		t.Errorf("bad max_tokens_param: %v", err)
	}
}

// The example's request controls are commented out for users to copy; with
// the comments removed the example must still load.
func TestExampleControlsUncommented(t *testing.T) {
	lines := strings.Split(ExampleYAML, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "agent:") })
	end := slices.Index(lines, "  system: |")
	if start < 0 || end < start {
		t.Fatal("example.yaml: the agent section is not where this test looks")
	}
	for i := start; i < end; i++ {
		if strings.HasPrefix(lines[i], "    #") {
			lines[i] = strings.Replace(lines[i], "# ", "", 1)
		}
	}
	c, err := Parse([]byte(strings.Join(lines, "\n")), env(map[string]string{"HFY_SERVER_SECRET": "s", "DATABASE_URL": "u"}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Agent.Settings
	if s.ReasoningEffort != "low" || s.MaxOutputTokens != 4096 || s.CallTimeout.D() != 90*time.Second || s.Extra["cache_control"] == nil {
		t.Fatalf("uncommented controls parsed as %+v", s)
	}
}
