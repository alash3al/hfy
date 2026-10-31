package svc

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

var uiHosts = []string{"cdn.example.com", "*.s3.amazonaws.com"}

func mustSanitize(t *testing.T, raw string, allow bool) (json.RawMessage, json.RawMessage, error) {
	t.Helper()
	card, qs, err := SanitizeUI(json.RawMessage(raw), uiHosts, allow)
	return card, qs, err
}

// A valid card survives with everything it showed and nothing it did not.
func TestSanitizeCardLimits(t *testing.T) {
	card, _, err := mustSanitize(t, `{
		"v":1,"type":"card","title":"Pizza","subtitle":"<b>hot</b>","body":"12 EUR `+strings.Repeat("x", 600)+`",
		"badge":"`+strings.Repeat("y", 30)+`","junk":{"nested":[1,2,3]},"page":12,
		"kv":[{"label":"Size","value":"Large"},{"label":"<i>Color</i>","value":"Red"},
			{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"},
			{"label":"","value":"dead"}],
		"image":"https://cdn.example.com/pizza.png","evil":"x",
		"buttons":[{"send":{"label":"Order now"}},{"link":{"label":"Menu","url":"https://cdn.example.com/m"}},{"host":{"label":"Pay","name":"payments","payload":{"amount":12}}},
			{"send":{"label":"Never shown"}},{"weird":{"x":1}},{"link":{"label":"Bad","url":"http://cdn.example.com/m"}},
			{"host":{"label":"Pay","name":"BAD NAME"}},{"host":{"label":"Pay","name":"payments","payload":[1,2]}}]
	}`, true)
	if err != nil {
		t.Fatalf("valid card: %v", err)
	}
	var c UICard
	if json.Unmarshal(card, &c) != nil {
		t.Fatalf("card: %s", card)
	}
	if c.V != 1 || c.Type != "card" || c.Title != "Pizza" || c.Subtitle != "<b>hot</b>" {
		t.Fatalf("title/subtitle kept as plain text: %+v", c)
	}
	if len([]rune(c.Body)) != maxUIBody || len([]rune(c.Badge)) != maxUIBadge {
		t.Fatalf("body/badge clipped: %d %d", len([]rune(c.Body)), len([]rune(c.Badge)))
	}
	for _, s := range []string{"junk", "page", "evil"} {
		if strings.Contains(string(card), `"`+s+`"`) {
			t.Fatalf("unknown key %s not stripped: %s", s, card)
		}
	}
	if len(c.KV) != maxUIKVRows || c.KV[0].Label != "Size" || c.KV[1].Label != "<i>Color</i>" {
		t.Fatalf("kv rows: %+v", c.KV)
	}
	if c.Image != "https://cdn.example.com/pizza.png" {
		t.Fatalf("image: %q", c.Image)
	}
	if len(c.Buttons) != maxUIButtons {
		t.Fatalf("buttons: %+v", c.Buttons)
	}
	if c.Buttons[0].Send.Label == "" || c.Buttons[0].Send.Label != "Order now" ||
		c.Buttons[1].Link == nil || c.Buttons[1].Link.URL != "https://cdn.example.com/m" ||
		c.Buttons[2].Host == nil || c.Buttons[2].Host.Name != "payments" || string(c.Buttons[2].Host.Payload) != `{"amount":12}` {
		t.Fatalf("buttons: %+v", c.Buttons)
	}
}

// Every limit gets its own mistake and its own outcome.
func TestSanitizeLimits(t *testing.T) {
	card := func(title, body string) string {
		return `{"v":1,"type":"card","title":` + title + `,"body":` + body + `}`
	}
	for _, c := range []struct {
		name, raw string
		wantErr   string
	}{
		{"oversized", card(`"t"`, `"`+strings.Repeat("b", 34000)+`"`), "exceeds"},
		{"wrong version", `{"v":2,"type":"card","title":"t"}`, "must be v1"},
		{"unknown type", `{"v":1,"type":"table","title":"t"}`, "carries no usable card"},
		{"missing type", `{"v":1,"title":"t"}`, "carries no usable card"},
		{"not an object", `[1,2]`, "not a JSON object"},
		{"not json", `not json`, "not a JSON object"},
		{"no title no body", `{"v":1,"type":"card","subtitle":"s"}`, "carries no usable card"},
		{"carousel without cards", `{"v":1,"type":"carousel"}`, "carries no usable card"},
		{"carousel of dead cards", `{"v":1,"type":"carousel","cards":[{"subtitle":"s"},{"badge":"b"}]}`, "carries no usable card"},
	} {
		_, _, err := mustSanitize(t, c.raw, false)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// Image rules: https only, small, and its host allow-listed (wildcards work);
// a dropped image never kills the card.
func TestSanitizeImages(t *testing.T) {
	for img, want := range map[string]string{
		"https://cdn.example.com/i.png":                        "https://cdn.example.com/i.png",
		"https://a.s3.amazonaws.com/i.png":                     "https://a.s3.amazonaws.com/i.png",
		"https://other.example.com/i.png":                      "",
		"http://cdn.example.com/i.png":                         "",
		"ftp://cdn.example.com/i.png":                          "",
		"https://cdn.example.com/" + strings.Repeat("p", 2100): "",
		"": "",
	} {
		raw := `{"v":1,"type":"card","title":"t","image":"` + img + `"}`
		card, _, err := mustSanitize(t, raw, false)
		if err != nil {
			t.Fatalf("%s: %v", img, err)
		}
		var c UICard
		json.Unmarshal(card, &c)
		if want == "" && c.Image != "" {
			t.Errorf("image must drop: %s got %q", img, c.Image)
		} else if want != "" && c.Image != img {
			t.Errorf("image kept: %s got %q", img, c.Image)
		}
	}
}

// The carousel keeps its usable cards, at most ten.
func TestSanitizeCarousel(t *testing.T) {
	var cards []string
	for i := range maxUICarousel + 3 {
		cards = append(cards, `{"title":"card `+strings.Repeat("x", i)+`"}`)
	}
	raw := `{"v":1,"type":"carousel","cards":[` + strings.Join(cards, ",") + `]}`
	card, _, err := mustSanitize(t, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var c UICarousel
	if json.Unmarshal(card, &c) != nil {
		t.Fatalf("carousel: %s", card)
	}
	if c.V != 1 || c.Type != "carousel" || len(c.Cards) != maxUICarousel {
		t.Fatalf("carousel: %+v", c)
	}
	mixed := `{"v":1,"type":"carousel","cards":[{"title":"one"},{"subtitle":"dead"},{"body":"plain"}]}`
	card, _, err = mustSanitize(t, mixed, false)
	if err != nil {
		t.Fatalf("mixed carousel: %v", err)
	}
	json.Unmarshal(card, &c)
	if len(c.Cards) != 2 || c.Cards[0].Title != "one" || c.Cards[1].Body != "plain" {
		t.Fatalf("mixed carousel: %+v", c)
	}
}

// Tool-triggered questions ride the same block: parsed, repaired and handed
// back only when this server's config allows asking.
func TestSanitizeQuestions(t *testing.T) {
	raw := `{"v":1,"type":"card","title":"t","questions":{"questions":[
		{"id":"size","text":"Which size?","type":"single","options":[{"value":"m"},{"value":"l"}]},
		{"text":"Color?","options":["Black","White"]}]}}`
	_, qs, err := mustSanitize(t, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Questions []Question `json:"questions"`
	}
	json.Unmarshal(qs, &got)
	if len(got.Questions) != 2 || got.Questions[0].ID != "size" || got.Questions[1].ID != "q2" {
		t.Fatalf("repaired questions: %s", qs)
	}
	if _, qs, _ := mustSanitize(t, raw, false); qs != nil {
		t.Fatalf("questions must drop when asking is off: %s", qs)
	}
	// questions that cannot be repaired drop quietly; the card stays
	bad := `{"v":1,"type":"card","title":"t","questions":{"questions":[]}}`
	if _, qs, err := mustSanitize(t, bad, true); err != nil || qs != nil {
		t.Fatalf("bad questions drop, card stays: %s %v", qs, err)
	}
}

// Control and format characters never reach a viewer: bidi overrides and
// zero-width junk (Cc/Cf) are stripped from every string; a body keeps its
// newlines.
func TestSanitizeStripsInvisible(t *testing.T) {
	raw := `{"v":1,"type":"card","title":"A\u202a\u202bB\u200b","body":"line one\nline two\n\rthree","badge":"\u202e12 EUR",
		"kv":[{"label":"Si\u200cze","value":"L"}],
		"buttons":[{"send":{"label":"Order\u200b"}},{"link":{"label":"Men\u200du","url":"\u200dhttps://cdn.example.com/m"}}]}`
	card, _, err := mustSanitize(t, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	var c UICard
	if json.Unmarshal(card, &c) != nil {
		t.Fatalf("card: %s", card)
	}
	for name, got := range map[string]string{
		"title": c.Title, "badge": c.Badge,
		"kv label": c.KV[0].Label, "kv value": c.KV[0].Value,
		"send": c.Buttons[0].Send.Label, "link": c.Buttons[1].Link.Label, "link url": c.Buttons[1].Link.URL,
	} {
		for _, r := range got {
			if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
				t.Errorf("%s keeps an invisible character: %q", name, got)
			}
		}
	}
	// only the body keeps newlines (checked by equality below)
	if c.Title != "AB" || c.Badge != "12 EUR" || c.KV[0].Label != "Size" ||
		c.Buttons[0].Send.Label != "Order" || c.Buttons[1].Link.URL != "https://cdn.example.com/m" {
		t.Errorf("scrubbed fields: %+v", c)
	}
	if c.Body != "line one\nline two\nthree" {
		t.Errorf("body: keep newlines, drop the rest: %q", c.Body)
	}
}

// A host button's payload is carried as it was sent: never a float64
// round-trip, so big integers stay exact.
func TestSanitizeHostPayloadBigInt(t *testing.T) {
	raw := `{"v":1,"type":"card","title":"t","buttons":[{"host":{"label":"Pay",
		"name":"payments","payload":{"amount": 9007199254740993, "id":"  padded  ","n":9007199254740991}}}]}` + "\n"
	card, _, err := mustSanitize(t, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var c UICard
	if json.Unmarshal(card, &c) != nil {
		t.Fatalf("card: %s", card)
	}
	if string(c.Buttons[0].Host.Payload) != `{"amount":9007199254740993,"id":"  padded  ","n":9007199254740991}` {
		t.Errorf("payload drifted: %s", c.Buttons[0].Host.Payload)
	}
}

// A label that scrubs to empty drops its button; it must not render as blank.
func TestSanitizeEmptyLabel(t *testing.T) {
	raw := `{"v":1,"type":"card","title":"t","buttons":[
		{"send":{"label":"\u200b\u202b"}},
		{"link":{"label":"Menu","url":"https://cdn.example.com/m"}},
		{"host":{"label":"\u200b","name":"payments"}},
		{"send":{"label":"Fine"}}]}`
	card, _, err := mustSanitize(t, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var c UICard
	json.Unmarshal(card, &c)
	if len(c.Buttons) != 2 || c.Buttons[0].Link == nil || c.Buttons[1].Send == nil || c.Buttons[1].Send.Label != "Fine" {
		t.Fatalf("buttons: %+v", c.Buttons)
	}
}

// A button is rebuilt from its one validated kind and nothing else. Extra
// kinds that fail validation are gone; a button validating to two kinds is
// dropped whole.
func TestSanitizeButtonKinds(t *testing.T) {
	raw := `{"v":1,"type":"card","title":"t","buttons":[
		{"send":{"label":"Keep"},"host":{"label":"\u202eevil","name":"ANY NAME","payload":{"a":1}},"link":{"label":"l","url":"http://x.example/m"}},
		{"send":{"label":"Sendy"},"link":{"label":"OK","url":"https://cdn.example.com/m"}},
		{"link":{"label":"OK","url":"https://cdn.example.com/m"},"host":{"label":"Pay","name":"payments","payload":{"a":1}}},
		{"host":{"label":"Pay","name":"payments","payload":{"a":1}},"send":{"label":"Sendy"}}]}`
	card, _, err := mustSanitize(t, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var c UICard
	if err := json.Unmarshal(card, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Buttons) != 1 || c.Buttons[0].Send == nil || c.Buttons[0].Send.Label != "Keep" {
		t.Fatalf("buttons: %+v", c.Buttons)
	}
	got, err := json.Marshal(c.Buttons)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); s != `[{"send":{"label":"Keep"}}]` {
		t.Errorf("buttons: %s", s)
	}
}

// A block that is only questions (no card, or a card that fails) pauses the
// turn with no card rather than dying.
func TestSanitizeQuestionsOnly(t *testing.T) {
	qs := `"questions":{"questions":[{"id":"size","text":"Which size?","type":"single","options":[{"value":"s"},{"value":"l"}]}]}`
	for _, raw := range []string{
		`{"v":1,` + qs + `}`,               // no card at all
		`{"v":1,"type":"card",` + qs + `}`, // titleless, bodyless
		`{"v":1,"type":"carousel","cards":[{"subtitle":"x"}],` + qs + `}` + " ", // every card unusable
	} {
		card, questions, err := mustSanitize(t, raw, true)
		if len(questions) == 0 || err != nil {
			t.Errorf("raw %s: questions=%s err=%v", raw, questions, err)
		}
		if card != nil {
			t.Errorf("raw %s: an unusable card must not survive: %s", raw, card)
		}
	}
	// asking off: a questions-only block simply has nothing
	card, questions, _ := mustSanitize(t, `{"v":1,`+qs+`}`, false)
	if card != nil || questions != nil {
		t.Fatalf("no asking, no questions: %s %s", card, questions)
	}
	// and the turn only pauses when the questions survive (agent side)
	if _, questions, err := mustSanitize(t, `{"v":1,`+qs+`}`, true); err != nil || len(questions) == 0 {
		t.Fatalf("questions-only pause: %v", err)
	}
}
