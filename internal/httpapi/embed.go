package httpapi

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alash3al/hfy/internal/files"
)

//go:embed embed
var embedFS embed.FS

// embeddedAssets is built once per process, at the first Server.
var embeddedAssets = sync.OnceValues(func() (*assets, error) {
	sub, err := fs.Sub(embedFS, "embed")
	if err != nil {
		return nil, err
	}
	start := time.Now()
	a, err := newAssets(sub, encoders)
	if err == nil {
		slog.Info("embed assets ready", "files", len(a.byName), "compress_ms", time.Since(start).Milliseconds())
	}
	return a, err
})

type embedConfig struct {
	Lang      string   `json:"lang"`
	Dir       string   `json:"dir"`
	Theme     string   `json:"theme"`
	Accent    string   `json:"accent"`
	AccentSet bool     `json:"accent_set"`
	Density   string   `json:"density"`
	Layout    string   `json:"layout"`
	ToolCalls string   `json:"tool_calls"`
	Parents   []string `json:"parents"`
	// Uploads tells the composer whether to offer attachments at all, and
	// which types and size to check before uploading (the server checks again).
	Uploads *embedUploads `json:"uploads"`
}

type embedUploads struct {
	Types    []string `json:"types"`
	MaxBytes int64    `json:"max_bytes"`
}

// embedUploads is nil when the page cannot upload (no filesystem configured).
func (s *Server) embedUploads() *embedUploads {
	if !s.fsEnabled() {
		return nil
	}
	return &embedUploads{Types: s.cfg.Filesystem.Types, MaxBytes: int64(s.cfg.Filesystem.MaxBytes)}
}

// embedPage is the document for this server's config, built on the first request.
type embedPage struct {
	once    sync.Once
	content *content
	csp     string
	err     error
}

func (s *Server) serveEmbed(w http.ResponseWriter, r *http.Request) {
	p := &s.page
	p.once.Do(func() { p.content, p.csp, p.err = s.buildPage() })
	if p.err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Security-Policy", p.csp)
	serveContent(w, r, p.content, "no-cache")
}

func (s *Server) buildPage() (*content, string, error) {
	raw := s.assets.template
	if raw == nil {
		return nil, "", errors.New("embed: no " + pageName)
	}
	parents := s.cfg.Server.CORSOrigins
	if parents == nil {
		parents = []string{}
	}
	payload, err := json.Marshal(embedConfig{
		Lang:      s.cfg.Embed.Lang,
		Dir:       s.cfg.Embed.Dir,
		Theme:     s.cfg.Embed.Theme,
		Accent:    s.cfg.Embed.Accent,
		AccentSet: s.cfg.Embed.AccentSet,
		Density:   s.cfg.Embed.Density,
		Layout:    s.cfg.Embed.Layout,
		ToolCalls: s.cfg.Embed.ToolCalls,
		Parents:   parents,
		Uploads:   s.embedUploads(),
	})
	if err != nil {
		return nil, "", err
	}
	body := bytes.Replace(raw, []byte("__HFY_CONFIG_JSON__"), payload, 1)

	frameAncestors := "'none'"
	if len(parents) > 0 {
		if slices.Contains(parents, "*") {
			frameAncestors = "*"
		} else {
			frameAncestors = strings.Join(parents, " ")
		}
	}
	// The one inline script (the config above) is allowed by its hash, so
	// script-src stays 'self' without 'unsafe-inline'.
	scriptHash := sha256.Sum256(append(append([]byte("window.__HFY_EMBED__="), payload...), ';'))
	connectSrc := "'self'"
	if s.cfg.Filesystem != nil {
		// Direct (S3) uploads/downloads go from the page to storage.
		if origins := files.StorageOrigins(s.cfg.Filesystem.DSN); len(origins) > 0 {
			connectSrc += " " + strings.Join(origins, " ")
		}
	}
	csp := "default-src 'self'; script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(scriptHash[:]) + "'; style-src 'self'; img-src 'self' data: https:; connect-src " + connectSrc + "; font-src 'self'; frame-ancestors " + frameAncestors
	c, err := newContent("text/html; charset=utf-8", body, true, s.assets.encs)
	return c, csp, err
}

// serveEmbedAsset serves every embedded file at two URLs: under its own name,
// where it revalidates (hosts pin these), and under its content hash, where it
// is immutable. An unknown name, or a hash that is not the current one, is a
// plain 404 and not a redirect: a redirect would answer a stale hashed URL
// with different bytes, and a page must get exactly the files it names.
func (s *Server) serveEmbedAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	hashed := strings.HasPrefix(name, assetsPrefix)
	a := s.assets.byName[name]
	if hashed {
		a = s.assets.byHashed[strings.TrimPrefix(name, assetsPrefix)]
	}
	if a == nil {
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cacheControl := "no-cache"
	if hashed {
		cacheControl = immutable
	}
	serveContent(w, r, a.content, cacheControl)
}
