package config

import (
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
	for i := 0; i < 200; i++ { // map-order bugs show up only some of the time
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
