package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// pageName is the embedded document. It is a template (the page config is
// injected), so it is served by serveEmbed and never as an asset.
const pageName = "index.html"

// The two kinds of embedded file: what a browser can still shrink, and what it cannot.
const (
	javascript = "text/javascript; charset=utf-8"
	stylesheet = "text/css; charset=utf-8"
)

var fileTypes = map[string]struct {
	ctype    string
	compress bool
}{
	".js":    {javascript, true},
	".mjs":   {javascript, true},
	".css":   {stylesheet, true},
	".html":  {"text/html; charset=utf-8", true},
	".json":  {"application/json", true},
	".svg":   {"image/svg+xml", true},
	".txt":   {"text/plain; charset=utf-8", true},
	".png":   {"image/png", false},
	".jpg":   {"image/jpeg", false},
	".webp":  {"image/webp", false},
	".woff2": {"font/woff2", false},
}

// representation is one exact byte sequence a client can be sent. Its ETag is
// the hash of those bytes: the one rule for every file and every encoding.
type representation struct {
	body []byte
	etag string
}

func newRepresentation(body []byte) representation {
	sum := sha256.Sum256(body)
	return representation{body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

// content is what one URL serves.
type content struct {
	ctype    string
	identity representation
}

func newContent(ctype string, raw []byte) (*content, error) {
	return &content{ctype: ctype, identity: newRepresentation(raw)}, nil
}

// asset is one embedded file.
type asset struct {
	name    string // path under embed/: "ui.js"
	content *content
}

// assets is the registry of every embedded file, built once from the
// embedded tree: nothing here names a file, so a new file is served as soon as
// it is in the directory.
type assets struct {
	byName   map[string]*asset
	template []byte // pageName, nil when the tree has none
}

func newAssets(fsys fs.FS) (*assets, error) {
	a := &assets{byName: map[string]*asset{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if name == pageName {
			a.template = raw
			return nil
		}
		ft, ok := fileTypes[strings.ToLower(path.Ext(name))]
		if !ok {
			ft.ctype = "application/octet-stream"
		}
		c, err := newContent(ft.ctype, raw)
		if err != nil {
			return fmt.Errorf("embed %s: %w", name, err)
		}
		a.byName[name] = &asset{name: name, content: c}
		return nil
	})
	return a, err
}

// serveContent answers GET and HEAD for c. Range is not supported: a Range
// header is ignored and the whole body is sent with 200. Headers the caller
// set before the call (a CSP) are kept, 304 included.
func serveContent(w http.ResponseWriter, r *http.Request, c *content, cacheControl string) {
	rep := c.identity
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", rep.etag)
	if etagMatches(r.Header.Values("If-None-Match"), rep.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", c.ctype)
	h.Set("Content-Length", strconv.Itoa(len(rep.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rep.body)
	}
}

// maxConditionalHeader bounds what is parsed: a longer If-None-Match is
// ignored (the full body is sent), never matched.
const maxConditionalHeader = 8 << 10

// etagMatches is the weak comparison of RFC 9110 for If-None-Match: a list of
// entity tags, W/ ignored, or "*".
func etagMatches(values []string, etag string) bool {
	total := 0
	for _, v := range values {
		total += len(v)
	}
	if total > maxConditionalHeader {
		return false
	}
	for _, v := range values {
		if strings.TrimSpace(v) == "*" {
			return true
		}
		for tag := range strings.SplitSeq(v, ",") {
			if strings.TrimPrefix(strings.Trim(tag, " \t"), "W/") == etag {
				return true
			}
		}
	}
	return false
}
