package svc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func TestResolveVars(t *testing.T) {
	decl := map[string]config.Var{
		"name":  {Required: true},
		"corp":  {Default: new("Acme")},
		"extra": {},
	}
	got, err := svc.ResolveVars(decl, map[string]string{"name": "alice", "undeclared": "x"})
	if err != nil || got["name"] != "alice" || got["corp"] != "Acme" || got["extra"] != "" || len(got) != 3 {
		t.Fatalf("declared vars from the grant, defaults, undeclared dropped: %v %v", got, err)
	}
	// the config made a var required after the grant was created: not the client's mistake
	_, err = svc.ResolveVars(decl, nil)
	var bad *svc.BadRequestError
	if err == nil || errors.As(err, &bad) || !strings.Contains(err.Error(), `required var "name"`) {
		t.Fatalf("missing required: %v", err)
	}
}

// A grant's vars are checked when the backend creates it: a typo or a
// missing required var is the backend's 400 then, not a failed turn later.
func TestCheckVars(t *testing.T) {
	decl := map[string]config.Var{"name": {Required: true}, "corp": {Default: new("Acme")}}
	cases := []struct {
		vars    map[string]string
		wantErr string // "" = valid
	}{
		{map[string]string{"name": "alice"}, ""},
		{map[string]string{"name": "alice", "nmae": "x"}, "vars.nmae is not declared by the agent"},
		{map[string]string{"corp": "x"}, "vars.name is required"},
		{nil, "vars.name is required"},
	}
	for _, c := range cases {
		err := svc.CheckVars(decl, c.vars)
		var bad *svc.BadRequestError
		if c.wantErr == "" && err != nil || c.wantErr != "" && (!errors.As(err, &bad) || err.Error() != c.wantErr) {
			t.Errorf("%v: want %q, got %v", c.vars, c.wantErr, err)
		}
	}
}

func TestValidateAttachments(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{Settings: config.Settings{Vision: true}},
		Attachments: &config.Attachments{AllowedHosts: []string{"cdn.example.com", "*.s3.amazonaws.com"}, Types: []string{"image/png", "application/pdf"}}}
	cfg.SetDefaults()
	ok := []types.Attachment{{URL: "https://cdn.example.com/a.png", Mime: "image/png"}, {URL: "https://b.s3.amazonaws.com/x.pdf", Mime: "application/pdf"}}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, ok); err != nil {
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
		if err := svc.ValidateAttachments(cfg, &cfg.Agent, []types.Attachment{a}); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	cfg.Agent.Settings.Vision = false
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, ok[:1]); err == nil || !strings.Contains(err.Error(), "vision") {
		t.Errorf("images need vision: %v", err)
	}
	cfg.Agent.Settings.Vision = true
	many := make([]types.Attachment, 11)
	for i := range many {
		many[i] = ok[0]
	}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, many); err == nil || !strings.Contains(err.Error(), "at most 10") {
		t.Errorf("11 attachments must be refused: %v", err)
	}
	cfg.Attachments = nil
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, ok); err == nil {
		t.Error("no attachments config means rejected")
	}
	// File attachments: no host/type/count checks here (httpapi checks the
	// filesystem config), but vision and url/file exclusivity still apply.
	fileOK := []types.Attachment{{File: "conversations/c/a.png", Mime: "image/png"}}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, fileOK); err != nil {
		t.Errorf("file-only with nil attachments config: %v", err)
	}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, []types.Attachment{{File: "conversations/c/a.png", URL: "https://cdn.example.com/a.png", Mime: "image/png"}}); err == nil {
		t.Error("file+url must be rejected")
	}
	cfg.Agent.Settings.Vision = false
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, fileOK); err == nil || !strings.Contains(err.Error(), "vision") {
		t.Errorf("file images need vision: %v", err)
	}
	cfg.Agent.Settings.Vision = true
	cfg.Attachments = &config.Attachments{AllowedHosts: []string{"cdn.example.com"}, Types: []string{"image/png", "application/pdf"}, MaxCount: 10}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, fileOK); err != nil {
		t.Errorf("file attachment with attachments config: %v", err)
	}
	// types is a filesystem concern, not an attachments one
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, []types.Attachment{{File: "conversations/c/a.gif", Mime: "image/gif"}}); err != nil {
		t.Errorf("file type outside attachments.types is httpapi's call, not svc's: %v", err)
	}
}
