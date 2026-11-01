package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
agents: {main: { model: p/m, settings: { context_window: 10 } }}
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
		"agents: {main: { model: nope/m, settings: { context_window: 10 } }}":                                                  `unknown provider "nope"`,
		"agents: {main: { model: p/m }}":                                                                                       "context_window",
		"agents: {main: { model: p/m, settings: { context_window: 1 }, system: 'hi {{who}}' }}":                                "{{who}}",
		"agents: {main: { model: p/m, settings: { context_window: 1 }, mcp: [ghost] }}":                                        `unknown server "ghost"`,
		"agents: {main: { model: p/m, settings: { context_window: 1 } }}\nsubagents: { main: { description: d, model: p/m } }": "reserved",
		"agents: {main: { model: p/m, settings: { context_window: 1 } }}\nsubagents: { a: { model: p/m } }":                    "description",
		"agents: {main: { model: p/m, settings: { context_window: 1 } }}\nsubagents: { now: { description: d, model: p/m } }":  "taken by the built-in tool hfy.now",
		"agents: {main: { model: p/m, settings: { context_window: 1 } }}\nauth: { webhook: http://hook }":                      `unknown key "auth"`,
		"agents: {main: { model: p/m, settings: { context_window: 1 } }}\nmcp: { hfy: { type: local, command: [x] } }":         "reserved for hfy's built-in tools",
	}
	for tail, want := range cases {
		_, err := Parse([]byte(base+tail), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
	if _, err := Parse([]byte("database: {url: u}\nagents: {main: {model: p/m}}"), env(nil)); err == nil || !strings.Contains(err.Error(), "server.secret") {
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
	for _, in := range []string{"lots", "", "-1", "1e3", "1.5MB", "9999999999GB", "9223372036854775807KB"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d: must be rejected", in, got)
		}
	}
	if got, err := ParseBytes("8191GB"); err != nil || got != 8191<<30 {
		t.Errorf("a large size still fits: %d %v", got, err)
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: postgres://x}\nproviders: {p: {url: http://x}}\n"
	for _, c := range []struct{ yaml, want string }{
		{"agents:\n  main:\n    model: p/m\n    settings: {context_window: 10}\n    aproval: {tools: [\"files.*\"]}\n", `line 8: unknown key "aproval" in config.agents.main`},
		{"agents: {main: {model: p/m, settings: {context_window: 10, temprature: 0.2}}}\n", `unknown key "temprature" in config.agents.main.settings`},
		{"mcp: {files: {type: local, comand: [x]}}\nagents: {main: {model: p/m, settings: {context_window: 10}}}\n", `unknown key "comand" in config.mcp.files`},
		{"agents: {main: {model: p/m, settings: {context_window: 10}}}\nretension: {trace: 1d}\n", `unknown key "retension" in config`},
	} {
		_, err := Parse([]byte(base+c.yaml), func(string) (string, bool) { return "", false })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("want %q, got %v", c.want, err)
		}
	}
}

func TestNamedAgents(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: postgres://x}\nproviders: {p: {url: http://x}}\n"
	cfg, err := Parse([]byte(base+"agents:\n  main: {model: p/m, settings: {context_window: 10}}\n  store: {model: p/small, system: 'Store {{name}}', vars: {name: {required: true}}, settings: {context_window: 5}, approval: {tools: [\"cart.*\"]}}\n"),
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
		"agents: {main: {model: p/m, settings: {context_window: 1}}, summarizer: {model: p/m, settings: {context_window: 1}}}\n":                                      "agents.summarizer: name is reserved",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: q/m, settings: {context_window: 1}}}\n":                                               `agents.x.model: unknown provider "q"`,
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m}}\n":                                                                              "agents.x.settings.context_window is required",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1}, system: '{{nope}}'}}\n":                           "agents.x.system uses {{nope}}",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1}}}\nsubagents: {x: {description: d, model: p/m}}\n": "subagents.x: name is reserved or taken",
	} {
		if _, err := Parse([]byte(base+yaml), func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
}

// An origin is accepted only as a browser writes it, so it can ever match.
func TestCORSOrigins(t *testing.T) {
	base := "database: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
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
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
	if _, err := Parse([]byte(base+"embed: {accent: red}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.accent") {
		t.Errorf("bad accent must fail naming embed.accent, got %v", err)
	}
	for _, layout := range Layouts {
		if c, err := Parse([]byte(base+"embed: {layout: "+layout+"}\n"), env(nil)); err != nil || c.Embed.Layout != layout {
			t.Errorf("layout %s must be accepted, got %v", layout, err)
		}
	}
	if _, err := Parse([]byte(base+"embed: {layout: chat}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.layout must be one of bubbles, transcript, whatsapp") {
		t.Errorf("unknown layout must fail naming embed.layout and the valid ones, got %v", err)
	}
	for _, density := range Densities {
		if c, err := Parse([]byte(base+"embed: {density: "+density+"}\n"), env(nil)); err != nil || c.Embed.Density != density {
			t.Errorf("density %s must be accepted, got %v", density, err)
		}
	}
	if _, err := Parse([]byte(base+"embed: {density: roomy}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.density") {
		t.Errorf("unknown density must fail naming embed.density, got %v", err)
	}
	for _, accent := range []string{"red", "#6366f", "#gggggg", "#6366f1x"} {
		for _, layout := range []string{"bubbles", "whatsapp"} {
			if _, err := Parse([]byte(base+"embed: {layout: "+layout+", accent: \""+accent+"\"}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.accent") {
				t.Errorf("%s accent %q must fail naming embed.accent, got %v", layout, accent, err)
			}
		}
	}
	c, err := Parse([]byte(base), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Embed.Dir != "auto" || c.Embed.Theme != "auto" || c.Embed.Accent != "#6366f1" || c.Embed.Density != "comfortable" || c.Embed.Layout != "bubbles" || c.Embed.ToolCalls != "summary" {
		t.Errorf("embed defaults: %+v", c.Embed)
	}
}

// AccentSet is what the whatsapp layout reads: an accent written in YAML is set,
// even the default indigo; an omitted accent is not.
func TestEmbedAccentSet(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
	for _, tc := range []struct {
		name   string
		embed  string
		lookup func(string) (string, bool)
		set    bool
		err    bool
	}{
		{"no embed block", "", env(nil), false, false},
		{"layout only", "embed: {layout: whatsapp}\n", env(nil), false, false},
		{"explicit indigo", "embed: {accent: \"#6366f1\"}\n", env(nil), true, false},
		{"explicit rose", "embed: {accent: \"#e11d48\"}\n", env(nil), true, false},
		{"empty string", "embed: {accent: \"\"}\n", env(nil), false, false},
		{"null", "embed: {accent: null}\n", env(nil), false, false},
		{"env sets it", "embed: {accent: \"${ACC}\"}\n", env(map[string]string{"ACC": "#e11d48"}), true, false},
		{"env empty value", "embed: {accent: \"${ACC}\"}\n", env(map[string]string{"ACC": ""}), false, false},
		{"env default empty", "embed: {accent: \"${ACC:-}\"}\n", env(nil), false, false},
		{"env missing", "embed: {accent: \"${ACC}\"}\n", env(nil), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(base+tc.embed), tc.lookup)
			if tc.err {
				if err == nil {
					t.Fatalf("want an error, got AccentSet=%v", c.Embed.AccentSet)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Embed.AccentSet != tc.set {
				t.Errorf("AccentSet = %v, want %v (accent %q)", c.Embed.AccentSet, tc.set, c.Embed.Accent)
			}
		})
	}
}

// SetDefaults can run again on a config it already filled; the default indigo
// must not turn into an explicit accent on the second call.
func TestEmbedAccentSetTwice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		embed Embed
		set   bool
	}{
		{"unset", Embed{}, false},
		{"explicit indigo", Embed{Accent: "#6366f1"}, true},
		{"explicit rose", Embed{Accent: "#e11d48"}, true},
	} {
		c := Config{Embed: tc.embed}
		c.SetDefaults()
		if c.Embed.AccentSet != tc.set {
			t.Errorf("%s: first SetDefaults AccentSet = %v, want %v", tc.name, c.Embed.AccentSet, tc.set)
		}
		c.SetDefaults()
		if c.Embed.AccentSet != tc.set || c.Embed.Accent == "" {
			t.Errorf("%s: second SetDefaults AccentSet = %v, want %v (accent %q)", tc.name, c.Embed.AccentSet, tc.set, c.Embed.Accent)
		}
	}
}

func TestMockProviderParses(t *testing.T) {
	y := `
server: { secret: s }
database: { url: u }
providers:
  p: { url: http://x }
  fake: { type: mock, mock: { reply: "Hello from mock" } }
agents: {main: { model: p/m, settings: { context_window: 10 } }}
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
agents: {main: { model: fake/m, settings: { context_window: 10 } }}
`
	_, err := Parse([]byte(y), env(nil))
	if err == nil || !strings.Contains(err.Error(), "providers.fake.mock.error_status") {
		t.Errorf("bad error_status must fail naming providers.fake.mock.error_status, got %v", err)
	}
}

func TestRemovedEmbeddingsKeyFailsWithTailoredError(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
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

const filesBase = agentsBase + "agents: {main: {model: p/m, settings: {context_window: 1}}}\n"

func TestFilesDefaults(t *testing.T) {
	c, err := Parse([]byte(filesBase+"files: {types: [image/png], dsn: file:///tmp/hfy-files}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Files; f.MaxBytes != 10<<20 || f.MaxCount != 10 || f.UploadTTL.D() != 5*time.Minute || f.DownloadTTL.D() != 15*time.Minute {
		t.Errorf("files defaults: %+v", f)
	}
	// The v1 quirk is gone: the URL source never moves the upload limit.
	c, err = Parse([]byte(filesBase+"files: {types: [image/png], allowed_hosts: [cdn.example.com], dsn: file:///tmp/hfy-files}\n"), env(nil))
	if err != nil || c.Files.MaxBytes != 10<<20 {
		t.Errorf("max_bytes with both sources: %v %+v", err, c.Files)
	}
}

func TestFilesPredicates(t *testing.T) {
	var none *Files
	if none.URLs() || none.Uploads() || (&Files{}).URLs() || (&Files{}).Uploads() {
		t.Error("nil and empty Files turn nothing on")
	}
	if f := (&Files{AllowedHosts: []string{"a.example"}}); !f.URLs() || f.Uploads() {
		t.Error("allowed_hosts alone is URL attachments only")
	}
	if f := (&Files{DSN: "file://"}); f.URLs() || !f.Uploads() {
		t.Error("dsn alone is uploads only")
	}
	if c, err := Parse([]byte(filesBase+"files: {types: [image/png], allowed_hosts: [cdn.example.com]}\n"), env(nil)); err != nil || !c.Files.URLs() || c.Files.Uploads() {
		t.Errorf("hosts-only config: %v", err)
	}
}

func TestFilesValidation(t *testing.T) {
	ok := "types: [image/png], dsn: file:///tmp/f"
	for name, tc := range map[string]struct{ files, want string }{
		"empty map":            {"{}", "files.types is required"},
		"empty map needs src":  {"{}", "files needs allowed_hosts (URL attachments) or dsn (uploads)"},
		"no source":            {"{types: [image/png]}", "files needs allowed_hosts (URL attachments) or dsn (uploads)"},
		"no types":             {"{dsn: file:///tmp/f}", "files.types is required"},
		"empty types":          {"{types: [], dsn: file:///tmp/f}", "files.types is required"},
		"scalar":               {"5", "decode config"},
		"list":                 {"[a]", "decode config"},
		"unknown nested":       {"{" + ok + ", dns: x}", `unknown key "dns" in config.files (known: allowed_hosts, download_ttl, dsn, max_bytes, max_count, public_url, types, upload_ttl)`},
		"case variant":         {"{" + ok + ", DSN: x}", `unknown key "DSN"`},
		"duplicate key":        {"{" + ok + ", dsn: file:///other}", "already defined"},
		"wildcard type":        {"{types: [image/*], dsn: file:///tmp/f}", `"image/*"`},
		"uppercase type":       {"{types: [IMAGE/PNG], dsn: file:///tmp/f}", `"IMAGE/PNG"`},
		"param type":           {"{types: [\"image/png; charset=x\"], dsn: file:///tmp/f}", "image/png; charset=x"},
		"bare type":            {"{types: [png], dsn: file:///tmp/f}", `"png"`},
		"empty type":           {"{types: [\"\"], dsn: file:///tmp/f}", "files.types"},
		"dup type":             {"{types: [image/png, image/png], dsn: file:///tmp/f}", `"image/png" is listed twice`},
		"host scheme":          {"{types: [image/png], allowed_hosts: [\"https://cdn.x.com\"]}", "files.allowed_hosts"},
		"host path":            {"{types: [image/png], allowed_hosts: [cdn.x.com/a]}", "files.allowed_hosts"},
		"host port":            {"{types: [image/png], allowed_hosts: [\"cdn.x.com:443\"]}", "files.allowed_hosts"},
		"host star":            {"{types: [image/png], allowed_hosts: [\"*\"]}", "files.allowed_hosts"},
		"host star dot":        {"{types: [image/png], allowed_hosts: [\"*.\"]}", "files.allowed_hosts"},
		"host tld wildcard":    {"{types: [image/png], allowed_hosts: [\"*.com\"]}", "files.allowed_hosts"},
		"host uppercase":       {"{types: [image/png], allowed_hosts: [CDN.x.com]}", "files.allowed_hosts"},
		"host trailing dot":    {"{types: [image/png], allowed_hosts: [\"cdn.x.com.\"]}", "files.allowed_hosts"},
		"host idn":             {"{types: [image/png], allowed_hosts: [bücher.de]}", "punycode"},
		"host empty":           {"{types: [image/png], allowed_hosts: [\"\"]}", "files.allowed_hosts"},
		"host bracket ipv6":    {"{types: [image/png], allowed_hosts: [\"[::1]\"]}", "files.allowed_hosts"},
		"host dup":             {"{types: [image/png], allowed_hosts: [a.x.com, a.x.com]}", "listed twice"},
		"max_bytes negative":   {"{" + ok + ", max_bytes: -1}", "invalid size"},
		"max_bytes text":       {"{" + ok + ", max_bytes: lots}", "invalid size"},
		"max_bytes float":      {"{" + ok + ", max_bytes: 1e3}", "invalid size"},
		"max_bytes overflow":   {"{" + ok + ", max_bytes: 9999999999GB}", "invalid size"},
		"max_count negative":   {"{" + ok + ", max_count: -1}", "files.max_count must be a whole number greater than 0"},
		"max_count zero":       {"{" + ok + ", max_count: 0}", "files.max_count must be a whole number greater than 0"},
		"max_count float":      {"{" + ok + ", max_count: 1.5}", "files.max_count must be a whole number greater than 0"},
		"max_count exp":        {"{" + ok + ", max_count: 1e3}", "files.max_count must be a whole number greater than 0"},
		"max_count text":       {"{" + ok + ", max_count: many}", "files.max_count must be a whole number greater than 0"},
		"max_count zero env":   {"{" + ok + ", max_count: \"${ZERO}\"}", "files.max_count must be a whole number greater than 0"},
		"max_bytes zero":       {"{" + ok + ", max_bytes: 0}", "files.max_bytes must be greater than 0"},
		"max_bytes zero unit":  {"{" + ok + ", max_bytes: 0MB}", "files.max_bytes must be greater than 0"},
		"max_bytes MiB":        {"{" + ok + ", max_bytes: 10MiB}", "invalid size"},
		"max_bytes decimal":    {"{" + ok + ", max_bytes: 1.5MB}", "invalid size"},
		"upload_ttl zero":      {"{" + ok + ", upload_ttl: 0s}", "files.upload_ttl must be greater than 0"},
		"upload_ttl bare 0":    {"{" + ok + ", upload_ttl: 0}", "files.upload_ttl must be greater than 0"},
		"download_ttl zero":    {"{" + ok + ", download_ttl: 0d}", "files.download_ttl must be greater than 0"},
		"dsn null tilde":       {"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: ~}", "files.dsn is empty"},
		"dsn null word":        {"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: null}", "files.dsn is empty"},
		"host wildcard ipv4":   {"{types: [image/png], allowed_hosts: [\"*.0.0.1\"]}", "files.allowed_hosts"},
		"host numeric name":    {"{types: [image/png], allowed_hosts: [1.2.3]}", "files.allowed_hosts"},
		"host numeric wild":    {"{types: [image/png], allowed_hosts: [\"*.1.2.3\"]}", "files.allowed_hosts"},
		"host numeric tld":     {"{types: [image/png], allowed_hosts: [cdn.example.123]}", "files.allowed_hosts"},
		"host hex last label":  {"{types: [image/png], allowed_hosts: [127.0.0.0x1]}", "files.allowed_hosts"},
		"ttl negative":         {"{" + ok + ", upload_ttl: -5m}", "files.upload_ttl"},
		"ttl too long":         {"{" + ok + ", upload_ttl: 2h}", "files.upload_ttl"},
		"ttl days":             {"{" + ok + ", download_ttl: 30d}", "files.download_ttl"},
		"dsn scheme":           {"{types: [image/png], dsn: ftp://bucket}", "files.dsn must use s3:// or file://"},
		"dsn mem":              {"{types: [image/png], dsn: mem://}", "files.dsn must use s3:// or file://"},
		"s3 without region":    {"{types: [image/png], dsn: \"s3://bucket\"}", "region=<region>"},
		"public_url bad":       {"{" + ok + ", public_url: ftp://x}", "files.public_url must be an http(s) URL"},
		"public_url no dsn":    {"{types: [image/png], allowed_hosts: [cdn.x.com], public_url: https://h.example}", "files.public_url needs files.dsn"},
		"dsn empty string":     {"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: \"\"}", "files.dsn is empty"},
		"dsn bare":             {"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: }", "files.dsn is empty"},
		"dsn empty env":        {"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: \"${EMPTY}\"}", "files.dsn is empty"},
		"dsn unset env":        {"{types: [image/png], dsn: \"${NOPE}\"}", "NOPE"},
		"public_url empty env": {"{" + ok + ", public_url: \"${EMPTY}\"}", ""},
	} {
		_, err := Parse([]byte(filesBase+"files: "+tc.files+"\n"), env(map[string]string{"EMPTY": "", "ZERO": "0"}))
		switch {
		case name == "public_url empty env":
			if err != nil {
				t.Errorf("%s: an empty public_url is just unset, got %v", name, err)
			}
		case err == nil || !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	for name, files := range map[string]string{
		"null":           "null",
		"bare":           "",
		"unset env host": "{types: [image/png], allowed_hosts: [\"${H:-cdn.x.com}\"]}",
		"host digits ok": "{types: [image/png], allowed_hosts: [v1.example.com, \"*.cdn2.example.com\", a1.b2]}",
		"s3":             "{types: [image/png], dsn: \"s3://b?region=eu-west-1\"}",
		"hosts":          "{types: [image/png, application/pdf, image/svg+xml], allowed_hosts: [localhost, 127.0.0.1, \"::1\", xn--bcher-kva.de, cdn.example.com, \"*.s3.amazonaws.com\"]}",
		"size forms":     "{" + ok + ", max_bytes: \"5 mb\"}",
		"size int":       "{" + ok + ", max_bytes: 4096}",
	} {
		if _, err := Parse([]byte(filesBase+"files: "+files+"\n"), env(nil)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c, _ := Parse([]byte(filesBase+"files:\n"), env(nil)); c.Files != nil {
		t.Error("a bare files: is off, as if omitted")
	}
	if c, _ := Parse([]byte(filesBase+"files: {"+ok+", max_bytes: 5 mb}\n"), env(nil)); c.Files.MaxBytes != 5<<20 {
		t.Errorf("5 mb: %+v", c.Files)
	}
	// Case variant of the block itself.
	if _, err := Parse([]byte(filesBase+"Files: {"+ok+"}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), `unknown key "Files"`) {
		t.Errorf("Files: %v", err)
	}
	// Absent (or null) is the default; only an explicit zero is refused.
	if c, err := Parse([]byte(filesBase+"files: {"+ok+", max_bytes: ~, max_count: , upload_ttl: null}\n"), env(nil)); err != nil || c.Files.MaxBytes != 10<<20 || c.Files.MaxCount != 10 || c.Files.UploadTTL.D() != 5*time.Minute {
		t.Errorf("null limits mean default: %v %+v", err, c.Files)
	}
	// A value that reaches the node check through a YAML alias is still checked.
	const aliased = "server: {secret: s, listen: &z \"\"}\nqueue: {workers: &zero 0}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
	for set, want := range map[string]string{
		"{types: [image/png], allowed_hosts: [cdn.x.com], dsn: *z}": "files.dsn is empty",
		"{" + ok + ", max_count: *zero}":                            "files.max_count must be a whole number greater than 0",
	} {
		if _, err := Parse([]byte(aliased+"files: "+set+"\n"), env(nil)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("alias %s: want %q, got %v", set, want, err)
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("x: &f {dsn: ''}\nfiles: *f\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if v := lookupKey(doc.Content[0], "files", "dsn"); v == nil || v.Value != "" {
		t.Errorf("lookupKey must follow an aliased files block: %v", v)
	}
}

// The v1 blocks fail startup with one tailored message each, whatever their
// shape or value, and an old key beats a mistake in the new block.
func TestRemovedAttachmentsAndFilesystemKeys(t *testing.T) {
	const (
		att = "attachments was removed in v2; merge it into files: allowed_hosts, types and max_count move there (attachments.max_bytes capped uploads only when filesystem.max_bytes was unset: set files.max_bytes to keep that limit)"
		fsm = "filesystem was removed in v2; merge it into files: dsn, public_url, types, max_bytes, upload_ttl and download_ttl move there unchanged"
	)
	if _, err := Parse([]byte(filesBase), env(nil)); err != nil {
		t.Fatalf("the base config must parse: %v", err)
	}
	line := strings.Count(filesBase, "\n") + 1
	shapes := map[string]string{
		"map":       " {types: [image/png], allowed_hosts: [a.example.com]}\n",
		"empty map": " {}\n",
		"null":      " null\n",
		"bare":      "\n",
		"scalar":    " yes\n",
		"list":      " [a]\n",
		"unset env": " ${NOPE}\n", // refused before ${VAR} expansion could fail first
	}
	for key, want := range map[string]string{"attachments": att, "filesystem": fsm} {
		for name, set := range shapes {
			for where, y := range map[string]string{
				"alone":     filesBase + key + ":" + set,
				"after":     filesBase + "files: {types: [image/png], dsn: file:///f}\n" + key + ":" + set,
				"bad files": filesBase + "files: {typos: 1}\n" + key + ":" + set, // old key wins over an earlier mistake
			} {
				l := line
				if where != "alone" {
					l++
				}
				_, err := Parse([]byte(y), env(nil))
				if err == nil || err.Error() != fmt.Sprintf("line %d: %s", l, want) {
					t.Errorf("%s %s (%s): want line %d: %q, got %v", key, name, where, l, want, err)
				}
			}
		}
	}
	// Both old keys: the first in file order is reported.
	_, err := Parse([]byte(filesBase+"filesystem: {}\nattachments: {}\n"), env(nil))
	if err == nil || !strings.HasSuffix(err.Error(), fsm) {
		t.Errorf("both keys: want the filesystem message first, got %v", err)
	}
	// Case variants are just unknown keys; nested ones are not special.
	for _, y := range []string{"Attachments: {}\n", "Filesystem: {}\n", "files: {attachments: 1}\n", "embed: {filesystem: 1}\n"} {
		_, err := Parse([]byte(filesBase+y), env(nil))
		if err == nil || !strings.Contains(err.Error(), "unknown key") || strings.Contains(err.Error(), "removed in v2") {
			t.Errorf("%q: want the generic unknown-key error, got %v", y, err)
		}
	}
}

func TestExampleFilesUncommented(t *testing.T) {
	lines := strings.Split(ExampleYAML, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "# files:") })
	if start < 0 {
		t.Fatal("example.yaml: no commented files: block")
	}
	for i := start; lines[i] != ""; i++ {
		lines[i] = strings.Replace(lines[i], "# ", "", 1)
	}
	c, err := Parse([]byte(strings.Join(lines, "\n")), env(map[string]string{"HFY_SERVER_SECRET": "s", "DATABASE_URL": "u"}))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Files; !f.URLs() || !f.Uploads() || f.MaxBytes != 10<<20 || f.MaxCount != 10 || len(f.Types) != 3 {
		t.Fatalf("uncommented files: %+v", f)
	}
}

// Server-owned approval policy: patterns are validated like agent ones, and
// a pattern naming only a core tool can never work (core tools never wait).
func TestMCPServerApproval(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
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
		y := plain + `agents: {main: {model: p/m, settings: {context_window: 1}, approval: {tools: ["` + p + `"]}}}` + "\n"
		if _, err := Parse([]byte(y), env(nil)); err == nil || !strings.Contains(err.Error(), "core tool and never needs approval") {
			t.Errorf("%q must be rejected, got %v", p, err)
		}
	}
	if _, err := Parse([]byte(plain+"agents: {main: {model: p/m, settings: {context_window: 1}, approval: {tools: [\"hfy.*\"]}}}\n"), env(nil)); err != nil {
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
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
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
	c, err := Parse([]byte(base+"agents: {main: {model: p/m, settings: {context_window: 1024, reasoning_effort: none, max_output_tokens: 512, call_timeout: 30s, extra: {cache_control: {type: ephemeral}, reasoning: {effort: high}}}}}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Main().Settings
	if s.ReasoningEffort != "none" || s.MaxOutputTokens != 512 || s.CallTimeout.D() != 30*time.Second ||
		s.Extra["reasoning"].(map[string]any)["effort"] != "high" || c.Providers["p"].MaxTokensParam != "max_tokens" {
		t.Fatalf("parsed: %+v", s)
	}
	c, err = Parse([]byte("server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x, max_tokens_param: max_completion_tokens}}\n"+
		"agents: {main: {model: p/m, settings: {context_window: 1}}}\n"), env(nil))
	if err != nil || c.Providers["p"].MaxTokensParam != "max_completion_tokens" {
		t.Fatalf("max_tokens_param: %v %+v", err, c.Providers["p"])
	}

	bad := map[string]string{
		"agents: {main: {model: p/m, settings: {context_window: 1, reasoning_effort: maybe}}}":                                                         "agents.main.settings.reasoning_effort must be one of none, minimal, low, medium, high, xhigh, max",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1, reasoning_effort: Low}}}":           "agents.x.settings.reasoning_effort",
		"subagents: {r: {description: d, model: p/m, settings: {reasoning_effort: x}}}":                                                                "subagents.r.settings.reasoning_effort",
		"summarizer: {model: p/m, settings: {context_window: 1, reasoning_effort: x}}":                                                                 "summarizer.settings.reasoning_effort",
		"agents: {main: {model: p/m, settings: {context_window: 1, max_output_tokens: -1}}}":                                                           "agents.main.settings.max_output_tokens must be greater than 0",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1, max_output_tokens: -1}}}":           "agents.x.settings.max_output_tokens",
		"summarizer: {model: p/m, settings: {context_window: 1, max_output_tokens: -2}}":                                                               "summarizer.settings.max_output_tokens",
		"agents: {main: {model: p/m, settings: {context_window: 1, call_timeout: -5s}}}":                                                               "agents.main.settings.call_timeout must not be negative",
		"subagents: {r: {description: d, model: p/m, settings: {call_timeout: -1s}}}":                                                                  "subagents.r.settings.call_timeout",
		"agents: {main: {model: p/m, settings: {context_window: 1, reasoning_efort: low}}}":                                                            `unknown key "reasoning_efort" in config.agents.main.settings`,
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {model: x}}}}":                                                               "agents.main.settings.extra.model is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {messages: []}}}}":                                                           "agents.main.settings.extra.messages is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {tools: []}}}}":                                                              "agents.main.settings.extra.tools is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {stream: false}}}}":                                                          "agents.main.settings.extra.stream is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1, extra: {stream_options: {}}}}}":     "agents.x.settings.extra.stream_options is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {n: 2}}}}":                                                                   "agents.main.settings.extra.n is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {Tools: []}}}}":                                                              "agents.main.settings.extra.Tools is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {MODEL: x}}}}":                                                               "agents.main.settings.extra.MODEL is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1, extra: {Stream: true}}}}":           "agents.x.settings.extra.Stream is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {Foo: 1, foo: 2}}}}":                                                         "agents.main.settings.extra: Foo and foo differ only in case",
		"subagents: {r: {description: d, model: p/m, settings: {extra: {model: x}}}}":                                                                  "subagents.r.settings.extra.model is set by hfy",
		"summarizer: {model: p/m, settings: {context_window: 1, extra: {messages: []}}}":                                                               "summarizer.settings.extra.messages is set by hfy",
		"agents: {main: {model: p/m, settings: {context_window: 1, reasoning_effort: low, extra: {reasoning_effort: high}}}}":                          "agents.main.settings.extra.reasoning_effort clashes with settings.reasoning_effort",
		"agents: {main: {model: p/m, settings: {context_window: 10, max_output_tokens: 9, extra: {max_completion_tokens: 5}}}}":                        "agents.main.settings.extra.max_completion_tokens clashes with settings.max_output_tokens",
		"agents: {main: {model: p/m, settings: {context_window: 10, max_output_tokens: 9, extra: {max_tokens: 5}}}}":                                   "agents.main.settings.extra.max_tokens clashes with settings.max_output_tokens",
		"agents: {main: {model: p/m, settings: {context_window: 10, max_output_tokens: 9, extra: {MAX_TOKENS: 5}}}}":                                   "agents.main.settings.extra.MAX_TOKENS clashes with settings.max_output_tokens",
		"agents: {main: {model: p/m, settings: {context_window: 1, reasoning_effort: low, extra: {Reasoning_Effort: high}}}}":                          "agents.main.settings.extra.Reasoning_Effort clashes with settings.reasoning_effort",
		"agents: {main: {model: p/m, settings: {context_window: 1, temperature: 0.2, extra: {temperature: 0.5}}}}":                                     "agents.main.settings.extra.temperature clashes with settings.temperature",
		"agents: {main: {model: p/m, settings: {context_window: 1}}, x: {model: p/m, settings: {context_window: 1, top_p: 0.9, extra: {top_p: 0.5}}}}": "agents.x.settings.extra.top_p clashes with settings.top_p",
		"agents: {main: {model: p/m, settings: {context_window: 1, extra: {bad: .inf}}}}":                                                              "agents.main.settings.extra: values must be JSON-serialisable",
		"summarizer: {model: p/m, settings: {context_window: 1, extra: {bad: .nan}}}":                                                                  "summarizer.settings.extra: values must be JSON-serialisable",
	}
	for tail, want := range bad {
		_, err := Parse([]byte(base+tail+"\n"), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", tail, want, err)
		}
	}
	// the agent's own context window is still required next to the controls
	if _, err := Parse([]byte(base+"agents: {main: {model: p/m, settings: {reasoning_effort: low}}}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "context_window") {
		t.Errorf("controls do not replace context_window: %v", err)
	}
	if _, err := Parse([]byte("server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x, max_tokens_param: tokens}}\n"+
		"agents: {main: {model: p/m, settings: {context_window: 1}}}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "providers.p.max_tokens_param must be") {
		t.Errorf("bad max_tokens_param: %v", err)
	}
}

// max_output_tokens must stay below context_window wherever both are set. Each
// block is checked on its own, and the error names it and both values. The
// want is the whole error, so a block that also gets the old clash error, or
// no error, fails the case.
func TestMaxOutputTokensBelowContextWindow(t *testing.T) {
	const main = "agents: {main: {model: p/m, settings: {context_window: 100}}}\n"
	agent := func(settings string) string {
		return agentsBase + "agents: {main: {model: p/m, settings: {context_window: 100}}, x: {model: p/m, settings: {" + settings + "}}}\n"
	}
	cases := map[string]struct{ yaml, want string }{
		"agent equal":            {agent("context_window: 100, max_output_tokens: 100"), "agents.x.settings: max_output_tokens (100) must be smaller than context_window (100)"},
		"agent greater":          {agent("context_window: 100, max_output_tokens: 101"), "agents.x.settings: max_output_tokens (101) must be smaller than context_window (100)"},
		"agent smaller":          {agent("context_window: 100, max_output_tokens: 99"), ""},
		"agent unset":            {agent("context_window: 100"), ""},
		"agent zero":             {agent("context_window: 100, max_output_tokens: 0"), ""},
		"agent negative":         {agent("context_window: 100, max_output_tokens: -1"), "agents.x.settings.max_output_tokens must be greater than 0 (omit it for no cap)"},
		"agent huge":             {agent("context_window: 100, max_output_tokens: 9223372036854775807"), "agents.x.settings: max_output_tokens (9223372036854775807) must be smaller than context_window (100)"},
		"agent huge, both":       {agent("context_window: 9223372036854775807, max_output_tokens: 9223372036854775806"), ""},
		"agent no window":        {agent("max_output_tokens: 100"), "agents.x.settings.context_window is required"},
		"agent extra max_tokens": {agent("context_window: 100, extra: {max_tokens: 5000}"), ""},
		"agent extra clash":      {agent("context_window: 100, max_output_tokens: 50, extra: {max_tokens: 5000}"), "agents.x.settings.extra.max_tokens clashes with settings.max_output_tokens: set it there, not in extra"},
		"subagent equal":         {agentsBase + main + "subagents: {r: {description: d, model: p/m, settings: {context_window: 100, max_output_tokens: 100}}}\n", "subagents.r.settings: max_output_tokens (100) must be smaller than context_window (100)"},
		"subagent greater":       {agentsBase + main + "subagents: {r: {description: d, model: p/m, settings: {context_window: 100, max_output_tokens: 200}}}\n", "subagents.r.settings: max_output_tokens (200) must be smaller than context_window (100)"},
		"subagent smaller":       {agentsBase + main + "subagents: {r: {description: d, model: p/m, settings: {context_window: 100, max_output_tokens: 99}}}\n", ""},
		"subagent no window":     {agentsBase + main + "subagents: {r: {description: d, model: p/m, settings: {max_output_tokens: 100}}}\n", ""},
		"summarizer equal":       {agentsBase + main + "summarizer: {model: p/m, settings: {context_window: 100, max_output_tokens: 100}}\n", "summarizer.settings: max_output_tokens (100) must be smaller than context_window (100)"},
		"summarizer greater":     {agentsBase + main + "summarizer: {model: p/m, settings: {context_window: 100, max_output_tokens: 200}}\n", "summarizer.settings: max_output_tokens (200) must be smaller than context_window (100)"},
		"summarizer smaller":     {agentsBase + main + "summarizer: {model: p/m, settings: {context_window: 100, max_output_tokens: 99}}\n", ""},
		"summarizer huge":        {agentsBase + main + "summarizer: {model: p/m, settings: {context_window: 100, max_output_tokens: 9223372036854775807}}\n", "summarizer.settings: max_output_tokens (9223372036854775807) must be smaller than context_window (100)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml), env(nil))
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Errorf("error = %q, want %q", got, c.want)
			}
		})
	}
}

// The example's request controls are commented out for users to copy; with
// the comments removed the example must still load.
func TestExampleControlsUncommented(t *testing.T) {
	lines := strings.Split(ExampleYAML, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "agents:") })
	end := slices.Index(lines, "    system: |")
	if start < 0 || end < start {
		t.Fatal("example.yaml: the agents section is not where this test looks")
	}
	for i := start; i < end; i++ {
		if strings.HasPrefix(lines[i], "      #") {
			lines[i] = strings.Replace(lines[i], "# ", "", 1)
		}
	}
	c, err := Parse([]byte(strings.Join(lines, "\n")), env(map[string]string{"HFY_SERVER_SECRET": "s", "DATABASE_URL": "u"}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Main().Settings
	if s.ReasoningEffort != "low" || s.MaxOutputTokens != 4096 || s.CallTimeout.D() != 90*time.Second || s.Extra["cache_control"] == nil {
		t.Fatalf("uncommented controls parsed as %+v", s)
	}
}

const agentsBase = "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\n"

func TestRemovedAgentKeyFailsWithTailoredError(t *testing.T) {
	const want = "agent was removed in v2; move it to agents.main"
	main := "agents: {main: {model: p/m, settings: {context_window: 1}}}\n"
	if _, err := Parse([]byte(agentsBase+main), env(nil)); err != nil {
		t.Fatalf("the base config must parse: %v", err)
	}
	for name, set := range map[string]string{
		"map":       "agent: {model: p/m, settings: {context_window: 1}}\n",
		"empty map": "agent: {}\n",
		"null":      "agent: null\n",
		"bare":      "agent:\n",
		"scalar":    "agent: yes\n",
		"list":      "agent: [a]\n",
		"unset env": "agent: ${NOPE}\n", // refused before ${VAR} expansion could fail first
	} {
		for where, y := range map[string]string{"alone": agentsBase + set, "next to agents": agentsBase + set + main} {
			_, err := Parse([]byte(y), env(nil))
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "line 4") {
				t.Errorf("%s (%s): want %q at line 4, got %v", name, where, want, err)
			}
		}
	}
	// agents first: the removed key still fires, whatever order the file has.
	_, err := Parse([]byte(agentsBase+main+"agent: {model: p/m}\n"), env(nil))
	if err == nil || !strings.Contains(err.Error(), "line 5: "+want) {
		t.Errorf("agents then agent: want %q at line 5, got %v", want, err)
	}
	// Keys match case-sensitively: a variant is just unknown.
	_, err = Parse([]byte(agentsBase+main+"Agent: {}\n"), env(nil))
	if err == nil || !strings.Contains(err.Error(), `unknown key "Agent"`) || strings.Contains(err.Error(), "removed in v2") {
		t.Errorf("case variant: want the generic unknown-key error, got %v", err)
	}
	// Only the top-level key is removed; a nested one is not special.
	for _, y := range []string{"embed: {agent: 1}\n", "agents: {main: {model: p/m, settings: {context_window: 1}, agent: x}}\n"} {
		_, err = Parse([]byte(agentsBase+y), env(nil))
		if err == nil || !strings.Contains(err.Error(), `unknown key "agent"`) || strings.Contains(err.Error(), "removed in v2") {
			t.Errorf("nested key in %q: want the generic unknown-key error, got %v", y, err)
		}
	}
}

func TestMainAgentRequired(t *testing.T) {
	const want = `agents.main is required (the default agent: a grant without "agent" runs it)`
	store := "store: {model: p/m, settings: {context_window: 1}}"
	for name, set := range map[string]string{
		"absent":       "",
		"null":         "agents: null\n",
		"bare":         "agents:\n",
		"empty map":    "agents: {}\n",
		"only store":   "agents: {" + store + "}\n",
		"case variant": "agents: {Main: {model: p/m, settings: {context_window: 1}}}\n",
	} {
		_, err := Parse([]byte(agentsBase+set), env(nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", name, want, err)
		}
	}
	// agents is a map: any other shape is a decode error, never a silent empty set.
	for name, set := range map[string]string{"list": "agents: [a]\n", "scalar": "agents: yes\n"} {
		if _, err := Parse([]byte(agentsBase+set), env(nil)); err == nil || !strings.Contains(err.Error(), "decode config") {
			t.Errorf("%s: want a decode error, got %v", name, err)
		}
	}
	if _, err := Parse([]byte(agentsBase+"agents: {main: {model: p/m, settings: {context_window: 1}}, "+store+"}\n"), env(nil)); err != nil {
		t.Errorf("main plus another agent must parse: %v", err)
	}
}

// yaml.v3 decodes "main:" (no value) into a nil *Agent; SetDefaults must
// replace it so nothing dereferences nil and Validate names what is missing.
func TestNilAgentEntryDoesNotPanic(t *testing.T) {
	for name, set := range map[string]string{
		"null":          "agents: {main: null}\n",
		"bare":          "agents:\n  main:\n",
		"empty map":     "agents: {main: {}}\n",
		"other is null": "agents: {main: {model: p/m, settings: {context_window: 1}}, store: }\n",
	} {
		_, err := Parse([]byte(agentsBase+set), env(nil))
		if err == nil || !strings.Contains(err.Error(), "model") {
			t.Errorf("%s: want the model error, got %v", name, err)
		}
	}
	c := &Config{Agents: map[string]*Agent{"main": nil}}
	c.SetDefaults()
	if c.Main() == nil {
		t.Error("SetDefaults must replace a nil entry")
	}
}

func TestAgentNamedDefaultsToMain(t *testing.T) {
	c, err := Parse([]byte(agentsBase+"agents:\n  main: {model: p/m, settings: {context_window: 1}}\n  store: {model: p/s, settings: {context_window: 1}}\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "main"} {
		if a, ok := c.AgentNamed(name); !ok || a != c.Main() || a.Model != "p/m" {
			t.Errorf("AgentNamed(%q) = %+v %v, want the main agent", name, a, ok)
		}
	}
	if a, ok := c.AgentNamed("store"); !ok || a.Model != "p/s" {
		t.Errorf("store: %+v %v", a, ok)
	}
	for _, name := range []string{"nope", "Main", "MAIN", "main "} {
		if a, ok := c.AgentNamed(name); ok || a != nil {
			t.Errorf("AgentNamed(%q) must not resolve, got %+v", name, a)
		}
	}
}

// One table for the messages a v1 config or a hostile agents block meets.
func TestAgentsErrorMessages(t *testing.T) {
	ok := "model: p/m, settings: {context_window: 1}"
	long := strings.Repeat("a", 1000)
	for _, c := range []struct{ name, yaml, want string }{
		{"agent removed", "agent: {" + ok + "}\n", "line 4: agent was removed in v2; move it to agents.main"},
		{"agent and agents", "agent: {" + ok + "}\nagents: {main: {" + ok + "}}\n", "line 4: agent was removed in v2; move it to agents.main"},
		{"agents then agent", "agents: {main: {" + ok + "}}\nagent: {}\n", "line 5: agent was removed in v2; move it to agents.main"},
		{"agents missing", "", `agents.main is required (the default agent: a grant without "agent" runs it)`},
		{"main is null", "agents: {main: null}\n", `agents.main.model "" must look like <provider>/<model>`},
		{"main is empty", "agents: {main: {}}\n", `agents.main.model "" must look like <provider>/<model>`},
		{"main has no window", "agents: {main: {model: p/m}}\n", "agents.main.settings.context_window is required"},
		{"main unknown provider", "agents: {main: {model: q/m, settings: {context_window: 1}}}\n", `agents.main.model: unknown provider "q"`},
		{"main typo", "agents: {main: {modle: p/m}}\n", `unknown key "modle" in config.agents.main`},
		{"duplicate main", "agents: {main: {" + ok + "}, main: {" + ok + "}}\n", "already defined"},
		{"space in name", "agents: {main: {" + ok + "}, 'a b': {" + ok + "}}\n", "agents.a b: name is reserved or invalid"},
		{"double underscore", "agents: {main: {" + ok + "}, a__b: {" + ok + "}}\n", "agents.a__b: name is reserved or invalid"},
		{"summarizer name", "agents: {main: {" + ok + "}, summarizer: {" + ok + "}}\n", "agents.summarizer: name is reserved or invalid"},
		{"var in name", "agents: {main: {" + ok + "}, '${X}': {" + ok + "}}\n", "agents.${X}: name is reserved or invalid"},
		{"empty name", "agents: {main: {" + ok + "}, '': {" + ok + "}}\n", "agents.: name is reserved or invalid"},
		{"subagent named main", "agents: {main: {" + ok + "}}\nsubagents: {main: {description: d, model: p/m}}\n", "subagents.main: name is reserved or taken by an agent"},
	} {
		_, err := Parse([]byte(agentsBase+c.yaml), env(nil))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	// Names with case and length are fine: a grant's "agent" is matched exactly.
	y := agentsBase + "agents: {main: {" + ok + "}, Store: {" + ok + "}, store: {" + ok + "}, " + long + ": {" + ok + "}}\n"
	c, err := Parse([]byte(y), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Store", "store", long} {
		if _, found := c.AgentNamed(n); !found {
			t.Errorf("agent %.20q not found", n)
		}
	}
	// ${VAR} still expands in values under agents.main.
	c, err = Parse([]byte(agentsBase+"agents: {main: {model: '${M}', settings: {context_window: 1}}}\n"), env(map[string]string{"M": "p/x"}))
	if err != nil || c.Main().Model != "p/x" {
		t.Errorf("${VAR} in agents.main.model: %v", err)
	}
}

// embed.custom_css is a path: Parse checks only its shape (the file is read by
// the server, once), and Load makes a relative path relative to the config file.
func TestEmbedCustomCSS(t *testing.T) {
	base := "server: {secret: s}\ndatabase: {url: u}\nproviders: {p: {url: http://x}}\nagents: {main: {model: p/m, settings: {context_window: 1}}}\n"
	for yaml, want := range map[string]string{
		"embed: {custom_css: /etc/hfy/brand.css}\n": "/etc/hfy/brand.css",
		"embed: {custom_css: brand.css}\n":          "brand.css",
		"embed: {custom_css: ../css/a b.css}\n":     "../css/a b.css",
		"embed: {custom_css: \"\"}\n":               "",
		"embed: {layout: whatsapp}\n":               "",
		"embed: {custom_css: \"${CSS_PATH}\"}\n":    "/srv/x.css",
		"embed: {custom_css: \"${CSS_PATH:-}\"}\n":  "/srv/x.css",
		"embed: {custom_css: \"${NOPE:-}\"}\n":      "", // empty after expansion: no custom CSS
	} {
		c, err := Parse([]byte(base+yaml), env(map[string]string{"CSS_PATH": "/srv/x.css"}))
		if err != nil {
			t.Errorf("%s: %v", yaml, err)
		} else if c.Embed.CustomCSS != want {
			t.Errorf("%s: got %q, want %q", yaml, c.Embed.CustomCSS, want)
		}
	}
	for _, v := range []string{"\".a{color:red}\"", "\"a\\nb.css\"", "https://cdn.example.com/a.css", "\"/a/}b.css\"", "\"a\\u0000b\""} {
		if _, err := Parse([]byte(base+"embed: {custom_css: "+v+"}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "embed.custom_css is the path of a CSS file") {
			t.Errorf("custom_css %s must be refused as not a path, got %v", v, err)
		}
	}
	if _, err := Parse([]byte(base+"embed: {custom_css: \"${NOPE}\"}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("an unset variable must fail naming it, got %v", err)
	}
	if _, err := Parse([]byte(base+"embed: {custom_style: x}\n"), env(nil)); err == nil || !strings.Contains(err.Error(), "custom_css") {
		t.Errorf("a typo must name the known keys, got %v", err)
	}

	dir := t.TempDir()
	for yaml, want := range map[string]string{
		"embed: {custom_css: brand.css}\n":      filepath.Join(dir, "brand.css"),
		"embed: {custom_css: ../up/a.css}\n":    filepath.Join(filepath.Dir(dir), "up", "a.css"),
		"embed: {custom_css: /abs/brand.css}\n": "/abs/brand.css",
		"embed: {layout: bubbles}\n":            "",
	} {
		file := filepath.Join(dir, "hfy.yaml")
		if err := os.WriteFile(file, []byte(base+yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(file)
		if err != nil {
			t.Errorf("Load %s: %v", yaml, err)
		} else if c.Embed.CustomCSS != want {
			t.Errorf("Load %s: got %q, want %q", yaml, c.Embed.CustomCSS, want)
		}
	}
}
