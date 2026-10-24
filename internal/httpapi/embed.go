package httpapi

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/alash3al/hfy/internal/files"
)

//go:embed embed/index.html embed/client.js embed/ui.js embed/ui.css embed/alpine.min.js
var embedFS embed.FS

type embedConfig struct {
	Lang      string   `json:"lang"`
	Dir       string   `json:"dir"`
	Theme     string   `json:"theme"`
	Accent    string   `json:"accent"`
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

func (s *Server) serveEmbed(w http.ResponseWriter, r *http.Request) {
	raw, err := embedFS.ReadFile("embed/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
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
		Density:   s.cfg.Embed.Density,
		Layout:    s.cfg.Embed.Layout,
		ToolCalls: s.cfg.Embed.ToolCalls,
		Parents:   parents,
		Uploads:   s.embedUploads(),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(scriptHash[:])+"'; style-src 'self'; img-src 'self' data: https:; connect-src "+connectSrc+"; font-src 'self'; frame-ancestors "+frameAncestors)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func serveEmbedAsset(name, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := embedFS.ReadFile("embed/" + name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(raw)
	})
}
