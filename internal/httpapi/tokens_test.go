package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
)

// internalTokens are the --hfy-* properties the stylesheets use but that are
// not the operator's to set: the UI writes them inline, per element.
var internalTokens = []string{"--hfy-p", "--hfy-sheet-pad"}

var (
	tokenRe    = regexp.MustCompile(`--hfy-[a-z0-9]+(?:-[a-z0-9]+)*`)
	tokenDefRe = regexp.MustCompile(`(--hfy-[a-z0-9]+(?:-[a-z0-9]+)*)\s*:`)
	docRowRe   = regexp.MustCompile("(?m)^\\| (`[^|]+`) \\| ([a-z]+) \\|")
	docHookRe  = regexp.MustCompile("(?m)^\\| (`[^|]+`) \\| ([^|]+) \\|$")
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// contractSection is the part of the reference between the contract's heading and the next one.
func contractSection(t *testing.T) string {
	t.Helper()
	ref := readFile(t, "../../docs/reference.md")
	_, rest, ok := strings.Cut(ref, "#### The `--hfy-*` tokens\n")
	if !ok {
		t.Fatal(`docs/reference.md has no "The --hfy-* tokens" section`)
	}
	section, _, _ := strings.Cut(rest, "\n### ")
	section, _, _ = strings.Cut(section, "\n#### ")
	return section
}

// The documented override surface and the stylesheets agree: every token in
// the reference is defined where it says, and every --hfy-* property the
// sheets define, use or the UI writes is documented or one of the internal ones.
func TestTokenContract(t *testing.T) {
	uiCSS := cssComment.ReplaceAllString(readFile(t, "embed/ui.css"), "")
	sheets := map[string]string{"all": uiCSS}
	all := uiCSS
	for _, layout := range config.Layouts[1:] {
		sheets[layout] = cssComment.ReplaceAllString(readFile(t, "embed/layouts/"+layout+".css"), "")
		all += sheets[layout]
	}

	documented := map[string]string{} // token -> layout
	for _, m := range docRowRe.FindAllStringSubmatch(contractSection(t), -1) {
		if _, ok := sheets[m[2]]; !ok {
			t.Errorf("reference: %s names layout %q, which is not one of all, %v", m[1], m[2], config.Layouts[1:])
		}
		for _, tok := range tokenRe.FindAllString(m[1], -1) {
			if prev, dup := documented[tok]; dup {
				t.Errorf("reference lists %s twice (%s, %s)", tok, prev, m[2])
			}
			documented[tok] = m[2]
		}
	}
	if len(documented) < 36 {
		t.Fatalf("only %d tokens parsed from the reference table", len(documented))
	}

	// every documented token is defined, in the sheet its row names
	for tok, layout := range documented {
		if !regexp.MustCompile(regexp.QuoteMeta(tok) + `\s*:`).MatchString(sheets[layout]) {
			t.Errorf("reference documents %s for %s, but that stylesheet does not define it", tok, layout)
		}
	}

	// every property the sheets define, use or the UI writes is documented or internal
	seen := map[string]string{}
	for _, m := range tokenDefRe.FindAllStringSubmatch(all, -1) {
		seen[m[1]] = "defined in CSS"
	}
	for _, tok := range tokenRe.FindAllString(readFile(t, "embed/ui.js"), -1) {
		seen[tok] = "written by ui.js"
	}
	for _, m := range regexp.MustCompile(`var\((--hfy-[a-z0-9-]+)`).FindAllStringSubmatch(all, -1) {
		if seen[m[1]] == "" {
			seen[m[1]] = "used with var() but never defined"
		}
	}
	for tok, how := range seen {
		if _, ok := documented[tok]; !ok && !slices.Contains(internalTokens, tok) {
			t.Errorf("%s (%s) is not in the reference table: document it, or add it to internalTokens", tok, how)
		}
	}
	for _, tok := range internalTokens {
		if _, ok := documented[tok]; ok {
			t.Errorf("%s is internal and documented", tok)
		}
		if seen[tok] == "" {
			t.Errorf("internalTokens lists %s, which nothing uses any more", tok)
		}
	}

	// the 36 variables measured in a real browser for the Dart client are all in the contract
	var measured struct {
		Variables map[string][]string `json:"variables"` // group -> names
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join("..", "..", "clients", "fixtures", "tokens.json"))), &measured); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, names := range measured.Variables {
		for _, tok := range names {
			n++
			if _, ok := documented[tok]; !ok {
				t.Errorf("tokens.json measures %s, which the reference does not document", tok)
			}
		}
	}
	if n < 36 {
		t.Fatalf("tokens.json lists %d variables", n)
	}
}

// The hooks table names every layout and density the config accepts.
func TestTokenContractHooks(t *testing.T) {
	rows := map[string]string{}
	for _, m := range docHookRe.FindAllStringSubmatch(contractSection(t), -1) {
		rows[strings.Trim(m[1], "`")] = m[2]
	}
	for attr, values := range map[string][]string{"data-layout": config.Layouts, "data-density": config.Densities, "data-theme": {"light", "dark"}} {
		for _, v := range values {
			if !strings.Contains(rows[attr], "`"+v+"`") {
				t.Errorf("the hooks table row %s does not list %q: %q", attr, v, rows[attr])
			}
		}
	}
	js := readFile(t, "embed/ui.js")
	for _, want := range []string{"dataset.theme", "dataset.density", "dataset.layout", "dataset.accentSet", "el.lang", "el.dir"} {
		if !strings.Contains(js, want) {
			t.Errorf("ui.js no longer sets %s, which the reference documents as a hook", want)
		}
	}
	if len(rows) < 5 {
		t.Errorf("hooks parsed: %v", rows)
	}
}
