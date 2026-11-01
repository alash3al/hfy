package types_test

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/types"
)

type brokenErr struct{}

func (brokenErr) Error() string { panic("boom") }

func TestErrorText(t *testing.T) {
	secret := "https://hook.example/p?token=SECRET"
	other := &url.Error{Op: "Get", URL: "http://other.example/x?k=SECRET2", Err: errors.New("refused")}
	var typedNil *url.Error
	cases := []struct {
		name string
		err  error
		want string // exact; empty means only the generic checks
	}{
		{"url error", &url.Error{Op: "Post", URL: secret, Err: errors.New("refused")}, `Post "<url>": refused`},
		{"joined second", errors.Join(&url.Error{Op: "Post", URL: secret, Err: errors.New("a")}, other), "Post \"<url>\": a\nGet \"<url>\": refused"},
		{"flattened", fmt.Errorf("deliver: %v", other), `deliver: Get "<url>": refused`}, //nolint:errorlint // %v flattens on purpose
		{"in the message", errors.New("upstream said see " + secret + " for more"), "upstream said see <url> for more"},
		{"double quoted", errors.New(`bad "` + secret + `": nope`), `bad "<url>": nope`},
		{"short url", &url.Error{Op: "Get", URL: "e", Err: errors.New("the error")}, `Get "<url>": the error`},
		{"short url unquoted", errors.New("the error e"), "the error e"},
		{"nul and utf8", errors.New("a\x00b\xed\xa0\x80c"), "ab\uFFFDc"},
		{"typed nil", typedNil, "error text unavailable"},
		{"broken Error method", brokenErr{}, "error text unavailable"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		got := types.ErrorText(c.err)
		if got != c.want || strings.Contains(got, "SECRET") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	got := types.ErrorText(errors.New(strings.Repeat("€", 100<<10)))
	if len(got) > types.MaxErrorBytes || !utf8.ValidString(got) || len(got) < types.MaxErrorBytes-3 {
		t.Errorf("bound: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}
