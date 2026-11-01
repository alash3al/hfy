package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alash3al/hfy/internal/config"
)

// Every yaml fence in the docs must be real config: known keys, valid values. A fence
// with both `server` and `database` is a whole config and is validated as written. Any
// other fence is a partial snippet: it is merged into a minimal valid config first, so
// it need not repeat the required keys. A fence that is deliberately invalid or only
// illustrative has a line that is exactly `# docs:skip-validate`. Not covered: the
// curl, HTML and JS snippets, which stay a review check.

var yamlFenceFiles = []string{"README.md", "docs/reference.md", "docs/llms.head.md"}

const skipValidate = "# docs:skip-validate"

const baseConfig = `
server: { secret: docs }
database: { url: postgres://docs }
providers: { openai: { url: "http://docs.invalid" } }
agents: { main: { model: openai/docs, settings: { context_window: 1000 } } }
`

// yamlFences returns the fences tagged yaml or yml (any letter case).
func yamlFences(md string) ([]fence, error) {
	_, all, err := scanFences(splitLines(md))
	if err != nil {
		return nil, err
	}
	var out []fence
	for _, f := range all {
		if f.lang == "yaml" || f.lang == "yml" {
			out = append(out, f)
		}
	}
	return out, nil
}

func mergeYAML(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeYAML(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// stubProviders adds an empty provider for every "<provider>/<model>" a snippet names
// and does not define, so an agent example need not repeat its provider.
func stubProviders(doc map[string]any) {
	providers, _ := doc["providers"].(map[string]any)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && k == "model" {
					if p, _, ok := strings.Cut(s, "/"); ok && providers[p] == nil {
						providers[p] = map[string]any{"url": "http://docs.invalid"}
					}
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
}

var errLineRe = regexp.MustCompile(`(?m)^line (\d+):`)

// validateFence reports whether the fence was skipped, and why it is not valid config.
// Error line numbers are the file's for a whole config; for a snippet they are the
// merged config's, and the error says so.
func validateFence(f fence) (skipped bool, err error) {
	for _, l := range splitLines(f.body) {
		if strings.TrimSpace(l) == skipValidate {
			return true, nil
		}
	}
	lookup := func(name string) (string, bool) {
		if name == "DATABASE_URL" {
			return "postgres://docs", true
		}
		return "docs-placeholder", true
	}
	var snippet any
	if err := yaml.Unmarshal([]byte(f.body), &snippet); err != nil {
		return false, err
	}
	top, ok := snippet.(map[string]any)
	if !ok {
		return false, fmt.Errorf("a config fence must be a mapping of top-level keys, got %T", snippet)
	}
	if top["server"] != nil && top["database"] != nil {
		_, err := config.Parse([]byte(f.body), lookup)
		if err != nil {
			err = errors.New(errLineRe.ReplaceAllStringFunc(err.Error(), func(m string) string {
				n, _ := strconv.Atoi(m[len("line ") : len(m)-1])
				return fmt.Sprintf("line %d:", f.line+n)
			}))
		}
		return false, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(baseConfig), &doc); err != nil {
		return false, err
	}
	mergeYAML(doc, top)
	stubProviders(doc)
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return false, err
	}
	if _, err = config.Parse(raw, lookup); err != nil {
		err = fmt.Errorf("partial snippet, line numbers are in the merged config: %w", err)
	}
	return false, err
}

func TestDocYAMLFences(t *testing.T) {
	for _, file := range yamlFenceFiles {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fences, err := yamlFences(string(b))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(fences) == 0 { // the extractor must not pass by finding nothing
			t.Fatalf("%s: no yaml fence found", file)
		}
		skippedN := 0
		for _, f := range fences {
			skipped, err := validateFence(f)
			if skipped {
				skippedN++
			}
			if err != nil {
				t.Errorf("%s:%d: yaml fence is not valid config: %v\n(add a `%s` line if it is meant to be invalid)", file, f.line, err, skipValidate)
			}
		}
		if skippedN == len(fences) {
			t.Errorf("%s: every yaml fence is skipped, so none is validated", file)
		}
		t.Logf("%s: %d yaml fences, %d skipped", file, len(fences), skippedN)
	}
}

func TestYAMLFences(t *testing.T) {
	cases := []struct {
		name, md string
		want     []string // bodies
		wantErr  bool
	}{
		{name: "language tags", md: "```yaml\na: 1\n```\n```yml\nb: 1\n```\n```YAML\nc: 1\n```\n```Yml\nd: 1\n```", want: []string{"a: 1", "b: 1", "c: 1", "d: 1"}},
		{name: "other languages ignored", md: "```sh\na\n```\n```json\n{}\n```\n```\nplain\n```\n```yaml-ish\nx\n```", want: nil},
		{name: "tilde fence", md: "~~~yaml\na: 1\n~~~", want: []string{"a: 1"}},
		{name: "yaml shown inside a four-backtick fence is not extracted", md: "````md\n```yaml\na: 1\n```\n````", want: nil},
		{name: "four-backtick yaml fence", md: "````yaml\na: 1\n````", want: []string{"a: 1"}},
		{name: "indented in a list item", md: "- x\n\n  ```yaml\n  a:\n    b: 1\n  ```", want: []string{"a:\n  b: 1"}},
		{name: "unterminated", md: "```yaml\na: 1\n", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, err := yamlFences(c.md)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			var got []string
			for _, f := range fs {
				got = append(got, f.body)
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestValidateFence(t *testing.T) {
	const whole = "server: { secret: \"${HFY_SERVER_SECRET}\" }\ndatabase: { url: \"${DATABASE_URL}\" }\n"
	cases := []struct {
		name, yaml string
		line       int // the fence's opening line in its file
		skipped    bool
		wantErr    string
	}{
		{name: "complete config", yaml: whole + "providers: { p: { url: http://x } }\nagents: { main: { model: p/m, settings: { context_window: 9 } } }"},
		{name: "partial snippet gets the base", yaml: "embed: { layout: transcript }"},
		{name: "partial agent merges into the base agent", yaml: "agents:\n  main:\n    system: hi\n    settings: { temperature: 0.2 }"},
		{name: "partial agent with an unknown provider", yaml: "agents:\n  main:\n    model: openrouter/vendor/model\n    settings:\n      max_output_tokens: 8192"},
		{name: "snippet adds a provider", yaml: "providers:\n  mock: { type: mock }\nagents: { main: { model: mock/demo } }"},
		{name: "env placeholders expand", yaml: "providers:\n  openai: { url: https://x, api_key: \"${OPENAI_API_KEY}\" }"},

		// a fence with server and database is a whole config: nothing is filled in for it
		{name: "complete-looking, no providers", yaml: whole + "agents: { main: { model: openai/x, settings: { context_window: 9 } } }", wantErr: `unknown provider "openai"`},
		{name: "complete-looking, no context_window", yaml: whole + "providers: { openai: { url: http://x } }\nagents: { main: { model: openai/x } }", wantErr: "context_window"},
		{name: "complete-looking, model names a missing provider", yaml: whole + "providers: { openai: { url: http://x } }\nagents: { main: { model: nope/x, settings: { context_window: 9 } } }", wantErr: `unknown provider "nope"`},
		{name: "complete-looking, no main agent", yaml: whole + "providers: { openai: { url: http://x } }", wantErr: "agents.main"},
		{name: "whole config: error line is the file's", yaml: whole + "providers: { p: { url: http://x } }\nagents: { main: { modle: x } }", line: 10, wantErr: "line 14: unknown key"},
		{name: "partial snippet: error says its lines are the merged config's", yaml: "embed: { layout: grid }", wantErr: "line numbers are in the merged config"},

		{name: "skip marker", yaml: "# docs:skip-validate\nnot: [valid", skipped: true},
		{name: "skip marker after the content, indented", yaml: "agents: { main: { modle: x } }\n  # docs:skip-validate  ", skipped: true},
		{name: "a longer comment is not the marker", yaml: "agents: { main: { modle: x } }\n# docs:skip-validated", wantErr: "unknown key"},
		{name: "a marker with a reason is not the marker", yaml: "agents: { main: { modle: x } }\n# docs:skip-validate because", wantErr: "unknown key"},
		{name: "skip text that is not a comment line", yaml: "agents: { main: { modle: x } } # docs:skip-validate", wantErr: "unknown key"},
		{name: "typo key", yaml: "agents: { main: { modle: x } }", wantErr: `unknown key "modle"`},
		{name: "top-level typo", yaml: "serverr: {}", wantErr: "unknown key"},
		{name: "removed key", yaml: "agent: { model: x }", wantErr: "agent was removed in v2"},
		{name: "invalid value", yaml: "embed: { layout: grid }", wantErr: "embed.layout"},
		{name: "list is not a config", yaml: "- a\n- b", wantErr: "must be a mapping"},
		{name: "empty", yaml: "# only a comment", wantErr: "must be a mapping"},
		{name: "broken yaml", yaml: "a: [", wantErr: "yaml"},
		{name: "duplicate key", yaml: "embed: { lang: en }\nembed: { lang: ar }", wantErr: "already defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skipped, err := validateFence(fence{body: c.yaml, line: c.line})
			if skipped != c.skipped {
				t.Fatalf("skipped = %v, want %v", skipped, c.skipped)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}
