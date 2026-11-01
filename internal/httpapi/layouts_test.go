package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
)

// pageWith serves /embed for a config with the given embed section.
func pageWith(t *testing.T, e config.Embed) string {
	t.Helper()
	cfg := &config.Config{Server: config.Server{Secret: "s"}, Embed: e}
	cfg.SetDefaults()
	w := get(New(cfg, nil, nil, nil).Handler(), http.MethodGet, "/embed")
	if w.Code != http.StatusOK {
		t.Fatalf("/embed: %d", w.Code)
	}
	return w.Body.String()
}

var (
	layoutLinkRe = regexp.MustCompile(`<link rel="stylesheet" href="([^"]*)" data-hfy-layout="([^"]*)">`)
	layoutNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// config.Layouts is the one list of layout names, and the layouts directory is
// its other half: the default is in ui.css, every other name is a stylesheet.
func TestLayoutsMatchConfig(t *testing.T) {
	a, _ := embeddedAssets()
	files := a.layouts()
	for _, name := range config.Layouts[1:] {
		if files[name] == "" {
			t.Errorf("config.Layouts names %q, which has no embed/layouts/%s.css", name, name)
		}
	}
	for name := range files {
		if !slices.Contains(config.Layouts[1:], name) {
			t.Errorf("embed/layouts/%s.css is not in config.Layouts, so no config can select it", name)
		}
		if !layoutNameRe.MatchString(name) {
			t.Errorf("layout name %q: lowercase letters, digits and dashes only", name)
		}
	}
	if len(config.Layouts) != len(files)+1 {
		t.Errorf("%d layouts, %d sheets, want one default and one sheet per other layout", len(config.Layouts), len(files))
	}
}

// The document names the configured layout's sheet right after ui.css, so the
// first paint has it; the default layout has no sheet; the page config lists
// every sheet, which is all a configure message can pick from.
func TestPageLayoutLink(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	ui := `href="/embed/` + assetsPrefix + a.byName["ui.css"].hashed + `">`
	for _, layout := range config.Layouts {
		page := pageWith(t, config.Embed{Layout: layout})
		if strings.Contains(page, "<!--hfy:") {
			t.Errorf("%s: a template marker is left in the page", layout)
		}
		links := layoutLinkRe.FindAllStringSubmatch(page, -1)
		if layout == config.Layouts[0] {
			if len(links) != 0 {
				t.Errorf("the default layout has no sheet, page has %v", links)
			}
		} else {
			if len(links) != 1 || links[0][2] != layout {
				t.Fatalf("%s: links %v", layout, links)
			}
			want := "/embed/" + assetsPrefix + a.byName["layouts/"+layout+".css"].hashed
			if links[0][1] != want {
				t.Errorf("%s: href %s, want %s", layout, links[0][1], want)
			}
			if i, j := strings.Index(page, ui), strings.Index(page, links[0][0]); i < 0 || j < i {
				t.Errorf("%s: the layout sheet must come after ui.css (ui.css at %d, sheet at %d)", layout, i, j)
			}
			w := get(h, http.MethodGet, want)
			raw, _ := os.ReadFile("embed/layouts/" + layout + ".css")
			if w.Code != 200 || w.Header().Get("Content-Type") != stylesheet || w.Header().Get("Cache-Control") != immutable || w.Body.String() != string(raw) {
				t.Errorf("%s: %d %q %q", want, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
			}
		}
		var manifest struct{ Layouts map[string]string }
		_, rest, _ := strings.Cut(page, "window.__HFY_EMBED__=")
		cfgJSON, _, _ := strings.Cut(rest, ";</script>")
		if err := json.Unmarshal([]byte(cfgJSON), &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Layouts) != len(config.Layouts)-1 {
			t.Errorf("%s: page layouts %v", layout, manifest.Layouts)
		}
		for name, href := range manifest.Layouts {
			if href != "/embed/"+assetsPrefix+a.byName["layouts/"+name+".css"].hashed {
				t.Errorf("manifest %s -> %s", name, href)
			}
		}
	}
	// A config that bypassed Validate with a name that has no sheet gets no link.
	if page := pageWith(t, config.Embed{Layout: "nope"}); layoutLinkRe.MatchString(page) {
		t.Error("a link for an unknown layout")
	}
}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRule    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	cssIsGroup = regexp.MustCompile(`:is\([^)]*\)`)
)

// A layout sheet styles its own layout and nothing else, and ui.css does not
// style a non-default layout except through the groups it shares with the
// default one (:is([data-layout="bubbles"],[data-layout="whatsapp"])). So
// the cascade does not depend on which sheet loads first, and layout CSS does
// not creep back into the first load.
func TestLayoutSheetsOwnTheirSelectors(t *testing.T) {
	for _, layout := range config.Layouts[1:] {
		src, err := os.ReadFile(path.Join("embed/layouts", layout+".css"))
		if err != nil {
			t.Fatal(err)
		}
		rules := cssRule.FindAllStringSubmatch(cssComment.ReplaceAllString(string(src), ""), -1)
		if len(rules) == 0 {
			t.Errorf("%s: no rules found", layout)
		}
		for _, m := range rules {
			if !strings.Contains(m[1], `[data-layout="`+layout+`"]`) {
				t.Errorf("%s.css: selector %q does not name the layout", layout, strings.TrimSpace(m[1]))
			}
		}
	}
	core, err := os.ReadFile("embed/ui.css")
	if err != nil {
		t.Fatal(err)
	}
	own := cssIsGroup.ReplaceAllString(string(core), "")
	for _, layout := range config.Layouts[1:] {
		if strings.Contains(own, `data-layout="`+layout+`"`) {
			t.Errorf("ui.css styles %s outside a shared :is() group: that rule belongs in layouts/%s.css", layout, layout)
		}
	}
}
