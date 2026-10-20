package svc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func def(s string) *string { return &s }

func TestResolveVars(t *testing.T) {
	decl := map[string]config.Var{
		"name":  {Required: true, MaxLength: 5},
		"corp":  {Default: def("Acme")},
		"extra": {},
	}
	got, err := svc.ResolveVars(decl, map[string]string{"name": "bob"}, nil)
	if err != nil || got["name"] != "bob" || got["corp"] != "Acme" || got["extra"] != "" {
		t.Fatalf("%v %v", got, err)
	}
	// trusted values override client values
	got, _ = svc.ResolveVars(decl, map[string]string{"name": "evil"}, map[string]string{"name": "alice", "undeclared": "x"})
	if got["name"] != "alice" || len(got) != 3 {
		t.Fatalf("trusted must win and undeclared trusted vars are dropped: %v", got)
	}
	for _, c := range []struct {
		client map[string]string
		want   string
	}{
		{nil, "missing required"},
		{map[string]string{"name": "toolong"}, "longer than 5"},
		{map[string]string{"name": "ok", "nope": "x"}, "unknown var"},
	} {
		_, err := svc.ResolveVars(decl, c.client, nil)
		var bad *svc.BadRequestError
		if !errors.As(err, &bad) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
}

func TestRender(t *testing.T) {
	got := svc.Render("Hi {{ name }} at {{corp}}, {{unknown}}", map[string]string{"name": "Bob", "corp": "Acme"})
	if got != "Hi Bob at Acme, {{unknown}}" {
		t.Fatal(got)
	}
	// substituted values are never re-expanded
	if got := svc.Render("{{a}}", map[string]string{"a": "{{b}}", "b": "x"}); got != "{{b}}" {
		t.Fatal(got)
	}
}

func TestValidateAttachments(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{Settings: config.Settings{Vision: true}},
		Attachments: &config.Attachments{AllowedHosts: []string{"cdn.example.com", "*.s3.amazonaws.com"}, Types: []string{"image/png", "application/pdf"}}}
	ok := []types.Attachment{{URL: "https://cdn.example.com/a.png", Mime: "image/png"}, {URL: "https://b.s3.amazonaws.com/x.pdf", Mime: "application/pdf"}}
	if err := svc.ValidateAttachments(cfg, ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]types.Attachment{
		"host not allowed":   {URL: "https://evil.com/a.png", Mime: "image/png"},
		"suffix trick":       {URL: "https://eviltext.s3.amazonaws.com.evil.com/a.png", Mime: "image/png"},
		"bare wildcard host": {URL: "https://s3.amazonaws.com/a.png", Mime: "image/png"},
		"type not allowed":   {URL: "https://cdn.example.com/a.gif", Mime: "image/gif"},
		"not http":           {URL: "file:///etc/passwd", Mime: "image/png"},
		"credentials":        {URL: "https://user:pw@cdn.example.com/a.png", Mime: "image/png"},
	}
	for name, a := range bad {
		if err := svc.ValidateAttachments(cfg, []types.Attachment{a}); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	cfg.Agent.Settings.Vision = false
	if err := svc.ValidateAttachments(cfg, ok[:1]); err == nil || !strings.Contains(err.Error(), "vision") {
		t.Errorf("images need vision: %v", err)
	}
	cfg.Attachments = nil
	if err := svc.ValidateAttachments(cfg, ok); err == nil {
		t.Error("no attachments config means rejected")
	}
}
