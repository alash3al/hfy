package svc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"unicode"
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
// (the caller drops it and warns) — unless valid questions survive it, in
// which case they are kept with no card, so a tool can just ask. Unknown
// fields are dropped, image urls are filtered against imageHosts, questions
// are dropped unless allowQuestions, and every string is Cleaned down to its
// visible characters.
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
	var cardErr error
	switch head.Type {
	case "card":
		if c, ok := sanitizeCard(raw, imageHosts); ok {
			cards = []UICard{c}
		} else {
			cardErr = fmt.Errorf("ui block carries no usable card (type %q)", head.Type)
		}
	case "carousel":
		cards, cardErr = sanitizeCarousel(raw, imageHosts)
	}
	if len(cards) == 0 {
		if questions != nil { // the card part is absent or unusable; the questions still pause
			return nil, questions, nil
		}
		if cardErr == nil {
			cardErr = fmt.Errorf("ui block carries no usable card (type %q)", head.Type)
		}
		return nil, nil, cardErr
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
// image the hosts do not allow, an empty label after scrubbing, an over-long
// string. A card with no title and no body is dropped.
func sanitizeCard(raw json.RawMessage, imageHosts []string) (UICard, bool) {
	var in uiCardIn
	if json.Unmarshal(raw, &in) != nil {
		return UICard{}, false
	}
	c := UICard{V: 1, Type: "card",
		Title:    cleanClip(in.Title, maxUITitle, false),
		Subtitle: cleanClip(in.Subtitle, maxUITitle, false),
		Body:     cleanClip(in.Body, maxUIBody, true),
		Badge:    cleanClip(in.Badge, maxUIBadge, false),
		Image:    uiImage(in.Image, imageHosts)}
	for _, r := range in.KV {
		if len(c.KV) == maxUIKVRows {
			break
		}
		c.KV = append(c.KV, UIKV{Label: cleanClip(r.Label, maxUIKVLabel, false), Value: cleanClip(r.Value, maxUIKVValue, false)})
	}
	for _, b := range in.Buttons {
		if len(c.Buttons) == maxUIButtons {
			break
		}
		// Every button is rebuilt from its validated parts (never the
		// decoded button); one carrying more than one kind is dropped whole.
		var send *UISend
		if b.Send != nil {
			if label := cleanClip(b.Send.Label, maxUIKVLabel, false); label != "" {
				send = &UISend{Label: label}
			}
		}
		link, host := uiLink(b.Link), uiHost(b.Host)
		kinds := 0
		for _, ok := range []bool{send != nil, link != nil, host != nil} {
			if ok {
				kinds++
			}
		}
		if kinds != 1 {
			continue
		}
		switch {
		case send != nil:
			c.Buttons = append(c.Buttons, UIButton{Send: send})
		case link != nil:
			c.Buttons = append(c.Buttons, UIButton{Link: link})
		case host != nil:
			c.Buttons = append(c.Buttons, UIButton{Host: host})
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
	if b == nil {
		return nil
	}
	b.Label = cleanClip(b.Label, maxUIKVLabel, false)
	b.URL = uiScrub(b.URL, false)
	if b.Label == "" || len(b.URL) > maxUILink || !uiHTTPs(b.URL) {
		return nil
	}
	return &UILink{Label: b.Label, URL: b.URL}
}

func uiHost(b *UIHost) *UIHost {
	if b == nil || !uiHostNameRe.MatchString(b.Name) {
		return nil
	}
	b.Label = cleanClip(b.Label, maxUIKVLabel, false)
	if b.Label == "" {
		return nil
	}
	out := &UIHost{Label: b.Label, Name: b.Name}
	if len(b.Payload) > 0 {
		// Never a map: a round-trip through any would lose the precision of
		// big integers. It must already be a JSON object; only compaction
		// rewrites it.
		var buf bytes.Buffer
		if !json.Valid(b.Payload) || !bytes.HasPrefix(bytes.TrimSpace(b.Payload), []byte("{")) ||
			json.Compact(&buf, b.Payload) != nil || buf.Len() > maxUIPayload {
			return nil
		}
		out.Payload = buf.Bytes()
	}
	return out
}

// uiImage keeps a https url whose host the config allows; anything else only
// costs the image, never the card.
func uiImage(s string, imageHosts []string) string {
	s = uiScrub(s, false)
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

// cleanClip is a scrub with a rune limit: control characters and formatters
// (bidi overrides, zero-width and friends) never survive; newlines only in a
// body. The clip follows the scrub, so invisible characters cannot push
// visible text past the limit.
func cleanClip(s string, n int, keepNL bool) string {
	s = uiScrub(s, keepNL)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

// uiScrub strips Unicode control (Cc) and format (Cf) characters, which
// render invisibly or can reorder display text; newlines survive where the
// caller says so (a card body only).
func uiScrub(s string, keepNL bool) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' && keepNL {
			return r
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, types.Clean(s))
}
