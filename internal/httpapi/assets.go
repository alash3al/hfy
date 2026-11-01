package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
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

// encoding is a Content-Encoding the server can pre-compress with.
type encoding struct {
	name     string
	compress func([]byte) ([]byte, error)
}

// encoders is every encoding offered, in order of preference. The bytes are
// compressed once, when the registry is built, never per request.
var encoders = []encoding{{"br", brotliBytes}, {"gzip", gzipBytes}}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// brotliQuality is the maximum: it runs once at startup (about 0.3 s for the
// whole UI) and it is what keeps the shell under its size budget (quality 9 is 8% bigger).
const brotliQuality = 11

func brotliBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	bw := brotli.NewWriterLevel(&buf, brotliQuality)
	if _, err := bw.Write(raw); err != nil {
		return nil, err
	}
	if err := bw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// representation is one exact byte sequence a client can be sent. Its ETag is
// the hash of those bytes: the one rule for every file and every encoding.
type representation struct {
	coding string // Content-Encoding; "" is the bytes as embedded
	body   []byte
	etag   string
}

func newRepresentation(coding string, body []byte) representation {
	sum := sha256.Sum256(body)
	return representation{coding: coding, body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

// content is what one URL serves.
type content struct {
	ctype    string
	identity representation
	encoded  []representation // in order of preference; only encodings that came out smaller
}

func newContent(ctype string, raw []byte, compressible bool, encs []encoding) (*content, error) {
	c := &content{ctype: ctype, identity: newRepresentation("", raw)}
	if !compressible {
		return c, nil
	}
	for _, e := range encs {
		body, err := e.compress(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		if len(body) < len(raw) {
			c.encoded = append(c.encoded, newRepresentation(e.name, body))
		}
	}
	return c, nil
}

// assetsPrefix is the URL namespace of content-hashed assets (under /embed/).
const assetsPrefix = "assets/"

// immutable is for URLs that name their content: the bytes behind one never change.
const immutable = "public, max-age=31536000, immutable"

// asset is one embedded file, served at two URLs: /embed/<name>, which
// revalidates, and the content-hashed /embed/assets/<stem>.<hash><ext>, which never changes.
type asset struct {
	name    string // path under embed/: "ui.js"
	hashed  string // path under /embed/assets/: "ui.3fa9c1d2e4b5.js"
	content *content
}

// hashedName puts the first 12 hex digits of the content's sha256 before the extension.
func hashedName(name string, raw []byte) string {
	sum := sha256.Sum256(raw)
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:6]) + ext
}

// assets is the registry of every embedded file, built once from the
// embedded tree: nothing here names a file, so a new file is served as soon as
// it is in the directory.
type assets struct {
	encs     []encoding
	byName   map[string]*asset
	byHashed map[string]*asset
	template []byte // pageName with every asset it references at its hashed URL; nil when the tree has none
}

func newAssets(fsys fs.FS, encs []encoding) (*assets, error) {
	a := &assets{encs: encs, byName: map[string]*asset{}, byHashed: map[string]*asset{}}
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
		if strings.HasPrefix(name, assetsPrefix) {
			return fmt.Errorf("embed %s: the %s directory is reserved for hashed URLs", name, assetsPrefix)
		}
		ft, ok := fileTypes[strings.ToLower(path.Ext(name))]
		if !ok {
			ft.ctype = "application/octet-stream"
		}
		c, err := newContent(ft.ctype, raw, ft.compress, encs)
		if err != nil {
			return fmt.Errorf("embed %s: %w", name, err)
		}
		as := &asset{name: name, hashed: hashedName(name, raw), content: c}
		a.byName[name] = as
		a.byHashed[as.hashed] = as
		return nil
	})
	if err != nil {
		return nil, err
	}
	if a.template != nil {
		a.template = a.rewrite(a.template)
	}
	return a, nil
}

// staleHash splits a hashed name into its stem and extension: ui.<12 hex>.js.
var staleHash = regexp.MustCompile(`^(.*)\.[0-9a-f]{12}(\.[^./]*)?$`)

// hashed finds the asset behind a path under /embed/assets/. A name with a
// hash that is not the current one still finds its file when the part before
// the hash and the extension name a known asset: stale is then true, and the
// caller serves the current bytes without letting anything cache them (a
// rolling deploy, or an old page open across an upgrade, asks for hashes this
// instance does not have). Anything else is nil.
func (a *assets) hashed(rest string) (as *asset, stale bool) {
	if as := a.byHashed[rest]; as != nil {
		return as, false
	}
	m := staleHash.FindStringSubmatch(rest)
	if m == nil {
		return nil, false
	}
	return a.byName[m[1]+m[2]], true
}

// rewrite points every quoted /embed/<name> reference in html at the asset's
// hashed URL. Only whole quoted values match, so ui.js never touches ui.js.map.
func (a *assets) rewrite(html []byte) []byte {
	for _, as := range a.byName {
		for _, q := range []string{`"`, `'`} {
			html = bytes.ReplaceAll(html, []byte(q+"/embed/"+as.name+q), []byte(q+"/embed/"+assetsPrefix+as.hashed+q))
		}
	}
	return html
}

// serveContent answers GET and HEAD for c, in the encoding the client accepts
// best. Range is not supported: a Range header is ignored and the whole body
// is sent with 200. Headers the caller set before the call (a CSP) are kept, 304 included.
func serveContent(w http.ResponseWriter, r *http.Request, c *content, cacheControl string) {
	h := w.Header()
	h.Add("Vary", "Accept-Encoding")
	rep := c.pick(r.Header.Values("Accept-Encoding"))
	if rep == nil {
		h.Set("Cache-Control", "no-store")
		http.Error(w, "no acceptable content encoding", http.StatusNotAcceptable)
		return
	}
	h.Set("Cache-Control", cacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", rep.etag)
	if etagMatches(r.Header.Values("If-None-Match"), rep.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", c.ctype)
	if rep.coding != "" {
		h.Set("Content-Encoding", rep.coding)
	}
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

// maxAcceptEncoding bounds what is parsed: a longer Accept-Encoding is
// ignored, which means identity.
const maxAcceptEncoding = 4 << 10

// pick chooses the representation for an Accept-Encoding header: the highest
// q wins, ties go to the earlier encoding, identity last. A missing, empty,
// oversized or unparseable header means identity. nil means the client
// refuses everything we have (406).
func (c *content) pick(header []string) *representation {
	q := parseAcceptEncoding(header)
	if len(q) == 0 {
		return &c.identity
	}
	var best *representation
	bestQ := 0
	try := func(rep *representation) {
		if w := weight(q, rep.coding); w > bestQ {
			best, bestQ = rep, w
		}
	}
	for i := range c.encoded {
		try(&c.encoded[i])
	}
	try(&c.identity)
	return best
}

// weight is the q (thousandths) the header gives a coding: its own entry, else
// "*", else 0. Identity is the exception: unlisted it stays acceptable, but
// last choice, so a client that lists gzip gets gzip.
func weight(q map[string]int, coding string) int {
	name := coding
	if name == "" {
		name = "identity"
	}
	if w, ok := q[name]; ok {
		return w
	}
	if w, ok := q["*"]; ok {
		return w
	}
	if coding == "" {
		return 1
	}
	return 0
}

// parseAcceptEncoding maps each coding to its q in thousandths. An entry that
// does not parse is dropped, and the first entry for a coding wins.
func parseAcceptEncoding(values []string) map[string]int {
	total := 0
	for _, v := range values {
		total += len(v)
	}
	if total > maxAcceptEncoding {
		return nil
	}
	q := map[string]int{}
	for _, v := range values {
		for entry := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(entry, ";")
			name = strings.ToLower(strings.Trim(name, " \t"))
			w, ok := 1000, name != ""
			for p := range strings.SplitSeq(params, ";") {
				k, val, isParam := strings.Cut(p, "=")
				if isParam && strings.EqualFold(strings.Trim(k, " \t"), "q") {
					w, ok = parseQ(strings.Trim(val, " \t"))
				}
			}
			if _, seen := q[name]; ok && !seen {
				q[name] = w
			}
		}
	}
	return q
}

// parseQ reads a qvalue: 0 or 1 with up to three decimals.
func parseQ(s string) (int, bool) {
	if s == "" || len(s) > 5 || (s[0] != '0' && s[0] != '1') || (len(s) > 1 && s[1] != '.') {
		return 0, false
	}
	w := int(s[0]-'0') * 1000
	scale := 100
	for _, d := range []byte(s[min(2, len(s)):]) {
		if d < '0' || d > '9' {
			return 0, false
		}
		w += int(d-'0') * scale
		scale /= 10
	}
	return w, w <= 1000
}
