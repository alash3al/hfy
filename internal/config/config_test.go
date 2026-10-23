package config

import (
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/types"
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
storage: { url: "${U}" }
queue: { workers: "${W}" }
providers: { p: { url: http://x } }
agent: { model: p/m, settings: { context_window: 10 } }
`
	c, err := Parse([]byte(y), env(map[string]string{"U": "pg://", "W": "7"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.Workers != 7 || c.Storage.URL != "pg://" {
		t.Fatalf("got %+v", c)
	}
}

func TestValidationErrors(t *testing.T) {
	base := `
server: { secret: s }
storage: { url: u }
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
	if _, err := Parse([]byte("storage: {url: u}\nagent: {model: p/m}"), env(nil)); err == nil || !strings.Contains(err.Error(), "server.secret") {
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
	base := "server: {secret: s}\nstorage: {url: postgres://x}\nproviders: {p: {url: http://x}}\n"
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

func TestAgentNamesMatchTypes(t *testing.T) {
	if MainAgent != types.AgentMain || summarizerName != types.AgentSummarizer {
		t.Fatal("config's agent names drifted from types")
	}
}

func TestNamedAgents(t *testing.T) {
	base := "server: {secret: s}\nstorage: {url: postgres://x}\nproviders: {p: {url: http://x}}\n" +
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
	base := "storage: {url: u}\nproviders: {p: {url: http://x}}\nagent: {model: p/m, settings: {context_window: 1}}\n"
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
