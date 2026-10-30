package svc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/types"
)

// The tool's _meta "hfy/ui" block, decoded, bounded and cleaned. Everything is
// plain strings on purpose: the embed UI renders with textContent only, so
// markup stays markup and nothing can inject.

const (
	maxUIBlock    = 32 << 10
	maxUIPayload  = 2 << 10 // a host button's payload
	maxUILink     = 2048    // an image or a link url, in bytes
	maxUITitle    = 120     // title/subtitle, in runes
	maxUIBody     = 500
	maxUIBadge    = 24
	maxUIKVRows   = 6
	maxUIKVLabel  = 40
	maxUIKVValue  = 120
	maxUIButtons  = 3
	maxUICarousel = 10
)

// UIKV is one row of a card's key: value table; exactly one kind fills a
// UIButton.
type UIKV struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type UISend struct {
	Label string `json:"label"`
}

type UILink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type UIHost struct {
	Label   string          `json:"label"`
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type UIButton struct {
	Send *UISend `json:"send,omitempty"`
	Link *UILink `json:"link,omitempty"`
	Host *UIHost `json:"host,omitempty"`
}

// UICard is the display card; UICarousel wraps one to ten of them.
type UICard struct {
	V        int        `json:"v"`
	Type     string     `json:"type"`
	Title    string     `json:"title,omitempty"`
	Subtitle string     `json:"subtitle,omitempty"`
	Body     string     `json:"body,omitempty"`
	Badge    string     `json:"badge,omitempty"`
	KV       []UIKV     `json:"kv,omitempty"`
	Image    string     `json:"image,omitempty"`
	Buttons  []UIButton `json:"buttons,omitempty"`
}

type UICarousel struct {
	V     int      `json:"v"`
	Type  string   `json:"type"`
	Cards []UICard `json:"cards"`
}

// SanitizeUI validates a tool's ui block and returns the display card(s),
// canonical JSON, plus its tool-triggered questions as {"questions": ...},
// repaired. A nil raw has nothing; a structurally unusable block is an error
// (the caller drops it and warns). Unknown fields are dropped, image urls are
// filtered against imageHosts, questions are dropped unless allowQuestions,
// and every string is Cleaned.
func SanitizeUI(raw json.RawMessage, imageHosts []string, allowQuestions bool) (card json.RawMessage, questions json.RawMessage, err error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if len(raw) > maxUIBlock {
		return nil, nil, fmt.Errorf("ui block of %d bytes exceeds %d", len(raw), maxUIBlock)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, nil, errors.New("ui block is not a JSON object")
	}
	var head struct {
		V         int             `json:"v"`
		Type      string          `json:"type"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, nil, fmt.Errorf("bad ui block: %w", err)
	}
	if head.V != 1 {
		return nil, nil, fmt.Errorf("ui block v %d must be v1", head.V)
	}
	if allowQuestions && len(head.Questions) > 0 {
		if qs, qerr := ParseQuestions(head.Questions); qerr == nil {
			questions = MarshalQuestions(qs)
		} else { // the card survives; only the questions are lost
			slog.Warn("tool's ui questions dropped", "err", qerr)
		}
	}
	var cards []UICard
	switch head.Type {
	case "card":
		if c, ok := sanitizeCard(raw, imageHosts); ok {
			cards = []UICard{c}
		}
	case "carousel":
		cards, err = sanitizeCarousel(raw, imageHosts)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(cards) == 0 {
		return nil, nil, fmt.Errorf("ui block carries no usable card (type %q)", head.Type)
	}
	if head.Type == "carousel" {
		if card, err = json.Marshal(UICarousel{V: 1, Type: "carousel", Cards: cards}); err != nil {
			return nil, nil, err
		}
		return card, questions, nil
	}
	card, merr := json.Marshal(cards[0])
	return card, questions, merr
}

// uiCardIn mirrors UICard's display fields, so unknown ones decode away; v and
// type live in SanitizeUI, where nesting is rejected.
type uiCardIn struct {
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle"`
	Body     string     `json:"body"`
	Badge    string     `json:"badge"`
	KV       []UIKV     `json:"kv"`
	Image    string     `json:"image"`
	Buttons  []UIButton `json:"buttons"`
}

// sanitizeCard decodes and bounds one card, dropping what does not survive: an
// image the hosts do not allow, a malformed button, an over-long string. A
// card with no title and no body is dropped.
func sanitizeCard(raw json.RawMessage, imageHosts []string) (UICard, bool) {
	var in uiCardIn
	if json.Unmarshal(raw, &in) != nil {
		return UICard{}, false
	}
	c := UICard{V: 1, Type: "card",
		Title:    cleanClip(in.Title, maxUITitle),
		Subtitle: cleanClip(in.Subtitle, maxUITitle),
		Body:     cleanClip(in.Body, maxUIBody),
		Badge:    cleanClip(in.Badge, maxUIBadge),
		Image:    uiImage(in.Image, imageHosts)}
	for _, r := range in.KV {
		if len(c.KV) == maxUIKVRows {
			break
		}
		c.KV = append(c.KV, UIKV{Label: cleanClip(r.Label, maxUIKVLabel), Value: cleanClip(r.Value, maxUIKVValue)})
	}
	for _, b := range in.Buttons {
		if len(c.Buttons) == maxUIButtons {
			break
		}
		switch {
		case b.Send != nil:
			if b.Send.Label == "" {
				continue
			}
			b.Send.Label = cleanClip(b.Send.Label, maxUIKVLabel)
			c.Buttons = append(c.Buttons, b)
		case uiLink(b.Link) != nil:
			c.Buttons = append(c.Buttons, UIButton{Link: uiLink(b.Link)})
		case uiHost(b.Host) != nil:
			c.Buttons = append(c.Buttons, UIButton{Host: uiHost(b.Host)})
		}
	}
	if c.Title == "" && c.Body == "" {
		return UICard{}, false
	}
	return c, true
}

// sanitizeCarousel keeps every usable card, at most ten; a carousel card never
// nests, a card is a card.
func sanitizeCarousel(raw json.RawMessage, imageHosts []string) ([]UICard, error) {
	var in struct {
		Cards []json.RawMessage `json:"cards"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil, errors.New("ui carousel is not usable")
	}
	var out []UICard
	for _, rc := range in.Cards {
		if len(out) == maxUICarousel {
			break
		}
		if c, ok := sanitizeCard(rc, imageHosts); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

var uiHostNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func uiLink(b *UILink) *UILink {
	if b == nil || b.Label == "" || len(b.URL) > maxUILink || !uiHTTPs(b.URL) {
		return nil
	}
	return &UILink{Label: cleanClip(b.Label, maxUIKVLabel), URL: b.URL}
}

func uiHost(b *UIHost) *UIHost {
	if b == nil || b.Label == "" || !uiHostNameRe.MatchString(b.Name) {
		return nil
	}
	out := &UIHost{Label: cleanClip(b.Label, maxUIKVLabel), Name: b.Name}
	if len(b.Payload) > 0 {
		var p map[string]any
		if json.Unmarshal(b.Payload, &p) != nil || p == nil {
			return nil
		}
		pb, err := json.Marshal(p)
		if err != nil || len(pb) > maxUIPayload {
			return nil
		}
		out.Payload = pb
	}
	return out
}

// uiImage keeps a https url whose host the config allows; anything else only
// costs the image, never the card.
func uiImage(s string, imageHosts []string) string {
	if s == "" || len(s) > maxUILink || !uiHTTPs(s) {
		return ""
	}
	u, _ := url.Parse(s)
	if !HostAllowed(imageHosts, u.Hostname()) {
		return ""
	}
	return s
}

// uiHTTPs accepts https urls only: an insecure image or link is dropped.
func uiHTTPs(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// cleanClip is Clean with a rune limit.
func cleanClip(s string, n int) string {
	s = types.Clean(s)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}
