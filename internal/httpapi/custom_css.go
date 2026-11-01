package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"unicode/utf8"
)

// customCSSName is the operator's stylesheet in the registry: served at
// /embed/custom.css and, immutable, at /embed/assets/custom.<hash>.css.
const customCSSName = "custom.css"

// maxCustomCSS is the most embed.custom_css may hold. The sheet rides on every
// viewer's first load, so it is capped like the layouts are, but wider.
const maxCustomCSS = 64 << 10

var (
	cssComments = regexp.MustCompile(`(?s)/\*.*?(?:\*/|$)`)
	cssImport   = regexp.MustCompile(`(?i)@import\b`)
	cssRemote   = regexp.MustCompile(`(?i)url\(\s*["']?(?:https?:)?//`)
)

// reason is err without the path the message already names.
func reason(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// readCapped reads r, and stops at one byte past the cap: a file that grows
// or never ends (a /proc entry reports size 0) is refused without being held.
func readCapped(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCustomCSS+1))
	if err == nil && len(raw) > maxCustomCSS {
		err = fmt.Errorf("larger than %d KiB", maxCustomCSS>>10)
	}
	return raw, err
}

// ReadCustomCSS reads the file embed.custom_css names. The path is the
// operator's, so a symlink is followed (a Kubernetes ConfigMap is one); what
// is refused is anything that is not a regular file (a directory, a device, a
// FIFO that would block), a file over 64 KiB, and text that is not UTF-8 or
// holds a NUL. A leading byte order mark is dropped. An @import is refused:
// the page's CSP (style-src 'self') would block it, and failing here beats a
// rule that silently does nothing. This is a startup check, not a sanitizer:
// the file is trusted, and is served as a stylesheet, never inline.
func ReadCustomCSS(path string) ([]byte, error) {
	fail := func(format string, a ...any) ([]byte, error) {
		return nil, fmt.Errorf("embed.custom_css %s: %s", path, fmt.Sprintf(format, a...))
	}
	// Stat before Open: opening a FIFO for reading waits for a writer.
	fi, err := os.Stat(path)
	if err != nil {
		return fail("%s", reason(err))
	}
	if fi.IsDir() {
		return fail("is a directory")
	}
	if !fi.Mode().IsRegular() {
		return fail("not a regular file (a device, pipe or socket)")
	}
	f, err := os.Open(path)
	if err != nil {
		return fail("%s", reason(err))
	}
	defer f.Close()
	raw, err := readCapped(f)
	if err != nil {
		return fail("%s", reason(err))
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return fail("not valid UTF-8")
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return fail("contains a NUL byte")
	}
	code := cssComments.ReplaceAll(raw, nil)
	if cssImport.Match(code) {
		return fail("@import is not supported (the page's CSP blocks it): put the rules in this file")
	}
	if cssRemote.Match(code) {
		slog.Warn("embed.custom_css loads a remote url(): the page's CSP allows https images only, and the host serving them sees every viewer", "path", path)
	}
	return raw, nil
}

// EnableCustomCSS serves css (what ReadCustomCSS returned) after the layout's,
// linked in the page so its rules win a tie. Call it before Handler, like
// EnableFilesystem. The sheet is fixed from here: a change needs a restart.
func (s *Server) EnableCustomCSS(css []byte) error {
	if s.built.Load() {
		panic("httpapi: EnableCustomCSS after Handler")
	}
	a, err := s.assets.with(customCSSName, css)
	if err != nil {
		return err
	}
	s.assets = a
	return nil
}
