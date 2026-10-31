package httpapi_test

// Shared fixtures between the JS embed UI and the future Dart SDK, verified
// against the real embed/ui.js under node. Fixture formats live in
// clients/fixtures/README.md.
//
// TestFixturesMatchJS:
//   - runs the real window.hfyEmbed() fold in node over every
//     clients/fixtures/events/*.json sequence and compares it with
//     clients/fixtures/expected/<name>.json;
//   - runs the real hfyMdParse over every markdown.json case;
//   - compares strings.json with the real HFY_STRINGS;
//   - sanity-checks tokens.json's shape (its values are browser-measured, so
//     they are never regenerated here — see README.md).
//
// -update-fixtures rewrites the files generated from ui.js: expected/*.json,
// strings.json and markdown.json (markdown inputs stay hand-authored; only the
// trees are regenerated).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateFixtures = flag.Bool("update-fixtures", false, "regenerate clients/fixtures generated files (expected/*.json, strings.json, markdown.json) from the real embed/ui.js")

// The JS half of the fixture test: it loads the REAL embed/ui.js under node with
// the same window/document stubs embed_test.go uses, then
//   - folds every clients/fixtures/events/*.json sequence through the real
//     window.hfyEmbed() fold and projects the folded state;
//   - parses every markdown.json case with the real hfyMdParse;
//   - dumps HFY_STRINGS.
//
// Results go to stdout as one JSON document; when update is true it also
// rewrites the generated fixture files from those results (canonical form:
// object keys sorted at every level, two-space indent).
// Env: HFY_UIJS = path to embed/ui.js, HFY_FIXTURES = path to clients/fixtures.
const fixturesHarnessJS = `
'use strict';
const fs = require('fs'), path = require('path');
const FIX = process.env.HFY_FIXTURES;
const ui = fs.readFileSync(process.env.HFY_UIJS, 'utf8');
const update = (() => { try { return JSON.parse(process.argv[2] || '{}').update === true; } catch (e) { return false; } })();

const RUN = '\n;return { hfyEmbed: window.hfyEmbed, hfyMdParse: hfyMdParse, HFY_STRINGS: HFY_STRINGS, setClient: function (v) { hfyClient = v; } };';
// Same stub level as embed_test.go: the fold never draws DOM, only reads state.
function loadUI() {
  const win = { addEventListener: function () {} };
  win.parent = win;
  const docs = { addEventListener: function () {}, querySelector: function () { return null },
    querySelectorAll: function () { return [] } };
  const f = new Function('window', 'document', 'navigator', 'requestAnimationFrame', ui + RUN);
  return f(win, docs, { language: 'en' }, function () {});
}

// Canonical form: object keys sorted at every level, everything else as-is.
function canon(x) {
  if (Array.isArray(x)) return x.map(canon);
  if (x && typeof x === 'object') { const o = {}; for (const k of Object.keys(x).sort()) o[k] = canon(x[k]); return o; }
  return x;
}
const pretty = v => JSON.stringify(canon(v), null, 2) + '\n';

function attachments(wire, blockId) {
  const list = wire[blockId] || [];
  if (!list.length) return undefined;
  return list.map(a => ({
    name: (a && (a.name || a.file || a.mime)) || '',
    mime: (a && a.mime) || '',
    hasUrl: !!(a && typeof a.url === 'string' && a.url),
    isImage: !!(a && a.url && /^image\//i.test(String((a && a.mime) || '')))
  }));
}

// The projection: every field the fold actually settles, none of the
// presentation-only ones (see clients/fixtures/README.md for the schema).
function projection(def, c, wireAtts, wireStructured, hasOlder) {
  const turns = c.turns.map(t => ({
    id: t.id,
    agent: t.agent,
    state: t.state,
    error: t.error || undefined,
    own: t.own,
    showTyping: t.showTyping,
    blocks: t.blocks.map(b => {
      const base = { id: b.id, kind: b.kind, iso: b.iso };
      if (b.kind === 'text') {
        base.role = b.isAgent ? 'agent' : 'user';
        base.ref = b.ref || '';
        base.text = b.text;
        base.streaming = b.streaming;
        base.attachments = attachments(wireAtts, b.id);
        return base;
      }
      if (b.kind === 'reasoning') {
        base.text = b.text;
        base.streaming = b.streaming;
        return base;
      }
      base.callId = b.callId;
      base.name = b.name;
      base.status = b.status;
      base.args = b.argsRaw == null ? null : b.argsRaw;
      base.content = b.content;
      base.structured = wireStructured[b.callId] != null ? wireStructured[b.callId] : undefined;
      base.isAsk = b.isAsk;
      base.questions = b.questions && b.questions.length ? b.questions : undefined;
      base.answers = b.finalAnswers || undefined;
      base.askState = b.hasQuestions ? b.askState : undefined;
      base.decision = b.decision || undefined;
      base.declineReason = b.declineReason || undefined;
      base.stopped = b.stopped;
      base.showApproval = b.showApproval;
      base.showDecided = b.showDecided;
      base.showPendingOwn = b.showPendingOwn;
      base.showPendingOther = b.showPendingOther;
      base.card = b.uiCard != null ? b.uiCard : undefined;
      base.cardButtonsLive = b.cardButtonsLive;
      return base;
    })
  }));
  return {
    name: def.name, path: def.path, viewer: def.viewer || null,
    hasOlder: hasOlder, showWelcome: c.showWelcome,
    pendingOwn: c.pendingOwn.map(b => b.callId), turns: turns
  };
}

// One wire sequence through the real fold. path 'live' folds with isHistory
// false (SSE frames); anything else folds as connect() delivers history.
// pages[] (newest first) are delivered through the component's REAL loadOlder,
// exactly as HfyClient#loadHistory fires them while the loader collects.
async function runScenario(def) {
  const api = loadUI();
  const comp = api.hfyEmbed();
  comp._loaded = true; // the real client has connected by the time events arrive
  const all = (def.events || []).concat(...(def.pages || []).map(p => p.events || []));
  // The fold keeps only derived attachment fields; the projection records the
  // wire's mime/structured for data the fold drops.
  const wireAtts = {}, wireStructured = {};
  all.forEach(e => {
    const d = e.data || {};
    if (e.type === 'message.text' && d.attachments) wireAtts[e.id] = d.attachments;
    if (e.type === 'tool_result' && d.structured != null) wireStructured[d.call_id] = d.structured;
  });
  const listeners = [];
  const client = {
    viewer: def.viewer || null,
    hasOlder: !!def.hasOlder,
    loadOlder: function () {
      (def.pages || []).forEach(p => {
        (p.events || []).forEach(e => listeners.forEach(fn => fn(e, true)));
        client.hasOlder = !!(p && p.next);
      });
    }
  };
  api.setClient(client);
  const isH = def.path !== 'live';
  (def.events || []).forEach(e => comp.fold(e, isH));
  if (def.pages && def.pages.length) {
    listeners.push(function (e, isHistory) { comp.fold(e, isHistory); }); // as hfyClient.init does
    await comp.loadOlder();
  }
  comp.relayout(); comp.syncDerived();
  return projection(def, comp, wireAtts, wireStructured, client.hasOlder);
}

(async function () {
  const events = {};
  const eventsDir = path.join(FIX, 'events');
  const files = fs.readdirSync(eventsDir).filter(f => f.endsWith('.json')).sort();
  for (const file of files) {
    const def = JSON.parse(fs.readFileSync(path.join(eventsDir, file), 'utf8'));
    const name = file.slice(0, -5);
    const proj = await runScenario(def);
    if (proj.name !== name || !def.viewer) { console.error('fixture ' + name + ': name/viewer header invalid'); process.exit(2); }
    events[name] = proj;
  }
  const strings = loadUI().HFY_STRINGS;

  // markdown: inputs are the hand-authored cases in markdown.json; trees come
  // from the real parser.
  const mdPath = path.join(FIX, 'markdown.json');
  const cases = JSON.parse(fs.readFileSync(mdPath, 'utf8'));
  const mdApi = loadUI();
  const md = cases.map(c => ({ name: c.name, input: c.input, tree: mdApi.hfyMdParse(c.input) }));

  if (update) {
    for (const name of Object.keys(events).sort()) fs.writeFileSync(path.join(FIX, 'expected', name + '.json'), pretty(events[name]));
    fs.writeFileSync(path.join(FIX, 'strings.json'), pretty({ en: strings.en, ar: strings.ar }));
    fs.writeFileSync(mdPath, pretty(md));
  }
  process.stdout.write(JSON.stringify({ events: events, strings: strings, markdown: md }));
})().catch(e => { console.error(e && e.stack || e); process.exit(1); });
`

type fixtureCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Tree  any    `json:"tree"`
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func writeCanonical(t *testing.T, path string, v any) {
	t.Helper()
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runFixturesHarness(t *testing.T, fixDir string, update bool) map[string]any {
	t.Helper()
	uiAbs, err := filepath.Abs(filepath.Join("embed", "ui.js"))
	if err != nil {
		t.Fatal(err)
	}
	fixAbs, err := filepath.Abs(fixDir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := json.Marshal(map[string]bool{"update": update})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "-e", fixturesHarnessJS, string(job))
	cmd.Env = append(os.Environ(), "TZ=UTC", "HFY_UIJS="+uiAbs, "HFY_FIXTURES="+fixAbs)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node fixture harness: %v\n%s", err, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("fixture harness output: %v", err)
	}
	return got
}

// canonJSON renders v in the canonical form the harness writes (sorted keys),
// so two projections compare as equal strings iff they are semantically equal.
func canonJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFixturesMatchJS(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("HFY_TEST_REQUIRE_DB") == "1" {
			t.Fatalf("node is required to fold the shared fixtures against embed/ui.js: %v", err)
		}
		t.Skip("node not found in PATH; the fixtures test folds the real ui.js under node")
	}
	if _, err := os.ReadFile("embed/ui.js"); err != nil {
		t.Fatal(err)
	}
	fixDir := filepath.Join("..", "..", "clients", "fixtures")

	// Header sanity on every event fixture before folding it: deterministic
	// projections need real RFC3339 stamps and canonical headers.
	eventDir := filepath.Join(fixDir, "events")
	entries, err := os.ReadDir(eventDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no event fixtures under clients/fixtures/events")
	}
	for _, name := range names {
		var def struct {
			Name   string           `json:"name"`
			Path   string           `json:"path"`
			Viewer *json.RawMessage `json:"viewer"`
			Events []struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CreatedAt string `json:"created_at"`
				V         *int   `json:"v"`
			} `json:"events"`
			Pages []struct {
				Events []struct {
					ID        string `json:"id"`
					Type      string `json:"type"`
					CreatedAt string `json:"created_at"`
					V         *int   `json:"v"`
				} `json:"events"`
			} `json:"pages"`
		}
		readJSON(t, filepath.Join(eventDir, name+".json"), &def)
		if def.Name != name {
			t.Errorf("%s: header name %q does not match file", name, def.Name)
		}
		if def.Path != "live" && def.Path != "history" {
			t.Errorf("%s: path %q is neither live nor history", name, def.Path)
		}
		if def.Viewer == nil {
			t.Errorf("%s: missing viewer", name)
		}
		var checkEnvs []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
			V         *int   `json:"v"`
		}
		checkEnvs = append(checkEnvs, def.Events...)
		for _, p := range def.Pages {
			checkEnvs = append(checkEnvs, p.Events...)
		}
		for _, e := range checkEnvs {
			if e.V != nil && *e.V != 1 {
				t.Errorf("%s: event %s has v=%d, wire is v=1", name, e.ID, *e.V)
			}
			if _, err := time.Parse(time.RFC3339, e.CreatedAt); err != nil {
				t.Errorf("%s: event %s created_at %q: %v (projections must be deterministic, so stamps are real RFC3339)", name, e.ID, e.CreatedAt, err)
			}
		}
	}

	got := runFixturesHarness(t, fixDir, *updateFixtures)

	// expected/<name>.json: the real fold's projection over each event file.
	events, _ := got["events"].(map[string]any)
	if events == nil {
		t.Fatal("harness returned no events")
	}
	for _, name := range names {
		gotP, ok := events[name]
		if !ok {
			t.Errorf("%s: harness produced no projection", name)
			continue
		}
		expectPath := filepath.Join(fixDir, "expected", name+".json")
		var want any
		if _, err := os.Stat(expectPath); os.IsNotExist(err) {
			t.Errorf("%s: missing expected/%s.json (run with -update-fixtures)", name, name)
			continue
		}
		readJSON(t, expectPath, &want)
		if !reflect.DeepEqual(gotP, want) {
			if *updateFixtures {
				writeCanonical(t, expectPath, gotP)
				continue
			}
			t.Errorf("%s: fold projection differs from expected/%s.json\ngot:  %s\nwant: %s",
				name, name, clip(canonJSON(t, gotP), 4000), clip(canonJSON(t, want), 4000))
		}
	}
	if *updateFixtures {
		for _, name := range names {
			if g, ok := events[name]; ok {
				writeCanonical(t, filepath.Join(fixDir, "expected", name+".json"), g)
			}
		}
	}

	// strings.json vs the real HFY_STRINGS.
	stringsGot, _ := got["strings"].(map[string]any)
	if stringsGot == nil {
		t.Fatal("harness returned no strings")
	}
	stringsPath := filepath.Join(fixDir, "strings.json")
	if _, err := os.Stat(stringsPath); os.IsNotExist(err) {
		t.Errorf("missing strings.json (run with -update-fixtures)")
	} else {
		var want map[string]any
		readJSON(t, stringsPath, &want)
		tables := map[string]any{"en": stringsGot["en"], "ar": stringsGot["ar"]}
		if !reflect.DeepEqual(tables, want) {
			if *updateFixtures {
				writeCanonical(t, stringsPath, tables)
			} else {
				t.Errorf("strings.json differs from the real HFY_STRINGS")
			}
		}
	}
	if *updateFixtures {
		writeCanonical(t, stringsPath, map[string]any{"en": stringsGot["en"], "ar": stringsGot["ar"]})
	}

	// markdown.json: real parser trees over the committed inputs.
	mdGot, _ := got["markdown"].([]any)
	if mdGot == nil {
		t.Fatal("harness returned no markdown cases")
	}
	mdPath := filepath.Join(fixDir, "markdown.json")
	if len(mdGot) < 60 {
		t.Errorf("markdown coverage: %d cases, want >= 60", len(mdGot))
	}
	if _, err := os.Stat(mdPath); os.IsNotExist(err) {
		t.Errorf("missing markdown.json (run with -update-fixtures)")
	} else {
		var want []fixtureCase
		readJSON(t, mdPath, &want)
		if len(want) != len(mdGot) {
			t.Errorf("markdown.json has %d cases, parser ran %d", len(want), len(mdGot))
		} else {
			for i, wc := range want {
				gc, _ := mdGot[i].(map[string]any)
				gName, _ := gc["name"].(string)
				gInput, _ := gc["input"].(string)
				if gName != wc.Name {
					t.Errorf("markdown case %d: name %q, file has %q", i, gName, wc.Name)
					continue
				}
				if gInput != wc.Input {
					t.Errorf("markdown case %s: input was edited without regenerating", wc.Name)
					continue
				}
				if !reflect.DeepEqual(gc["tree"], wc.Tree) {
					if *updateFixtures {
						continue // rewritten wholesale below
					}
					t.Errorf("markdown case %s: parser tree differs from committed tree", wc.Name)
				}
			}
		}
	}
	if *updateFixtures {
		cases := make([]map[string]any, 0, len(mdGot))
		for _, c := range mdGot {
			cm, _ := c.(map[string]any)
			cases = append(cases, cm)
		}
		writeCanonical(t, mdPath, cases)
	}

	// tokens.json is measured in a real browser (see README.md); the test only
	// guards its shape so a regeneration can't silently lose required sections.
	tokensPath := filepath.Join(fixDir, "tokens.json")
	var tokens map[string]any
	if _, err := os.Stat(tokensPath); os.IsNotExist(err) {
		t.Errorf("missing tokens.json (browser-measured, see clients/fixtures/README.md)")
	} else {
		readJSON(t, tokensPath, &tokens)
		for _, key := range []string{"method", "themes", "density", "variables"} {
			if _, ok := tokens[key]; !ok {
				t.Errorf("tokens.json missing %q", key)
			}
		}
		themes, _ := tokens["themes"].(map[string]any)
		for _, th := range []string{"light", "dark"} {
			tm, _ := themes[th].(map[string]any)
			if len(tm) == 0 {
				t.Errorf("tokens.json themes.%s missing", th)
			}
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:n], len(s))
}
