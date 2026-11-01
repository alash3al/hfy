package httpapi

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"

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
	return newAssets(sub)
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
	c, err := newContent("text/html; charset=utf-8", body)
	return c, csp, err
}

// serveEmbedAsset serves every embedded file under its own name; an unknown
// name is a plain 404.
func (s *Server) serveEmbedAsset(w http.ResponseWriter, r *http.Request) {
	a := s.assets.byName[r.PathValue("name")]
	if a == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serveContent(w, r, a.content, "no-cache")
}
