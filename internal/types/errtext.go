package types

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// MaxErrorBytes bounds ErrorText.
const MaxErrorBytes = 2 << 10

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"']*[^\s"':,.;)]`)

// ErrorText is err as safe to store and to show: valid UTF-8 with no NUL
// (Postgres rejects it), at most MaxErrorBytes cut on a rune boundary, and
// with every URL replaced by <url>, since a URL's path or query can be a
// secret. A broken Error method (a typed-nil error) gives a fixed text.
func ErrorText(err error) (text string) {
	if err == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			text = "error text unavailable"
		}
	}()
	msg := err.Error()
	if ue, ok := errors.AsType[*url.Error](err); ok && ue != nil && ue.URL != "" {
		// Quoted, as url.Error prints it, so a short URL cannot match elsewhere.
		msg = strings.ReplaceAll(msg, strconv.Quote(ue.URL), `"<url>"`)
	}
	msg = urlInText.ReplaceAllString(Clean(msg), "<url>")
	if len(msg) > MaxErrorBytes {
		msg = strings.ToValidUTF8(msg[:MaxErrorBytes], "")
	}
	return msg
}
