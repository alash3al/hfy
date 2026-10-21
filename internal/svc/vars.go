package svc

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/types"
)

// BadRequestError is a client mistake (HTTP 400).
type BadRequestError struct{ Msg string }

func (e *BadRequestError) Error() string { return e.Msg }

func badf(format string, a ...any) error { return &BadRequestError{fmt.Sprintf(format, a...)} }

// ResolveVars fills the declared vars from the auth webhook's: vars render
// into the system prompt, so only the app's backend sets them, never a
// client. Undeclared webhook vars are dropped and defaults fill the gaps; a
// required var the webhook left out is the webhook's mistake (a server error,
// not the client's).
func ResolveVars(decl map[string]config.Var, webhook map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for name, d := range decl {
		v, ok := webhook[name]
		switch {
		case ok:
			out[name] = v
		case d.Required:
			return nil, fmt.Errorf("the auth webhook did not supply the required var %q", name)
		case d.Default != nil:
			out[name] = *d.Default
		default:
			out[name] = ""
		}
	}
	return out, nil
}

var placeholder = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// Render substitutes {{var}} placeholders; unknown names are left as written.
func Render(tmpl string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// ValidateAttachments checks references at post time, so a bad attachment is a
// 400 and not a failed turn.
func ValidateAttachments(cfg *config.Config, agent *config.Agent, atts []types.Attachment) error {
	if len(atts) == 0 {
		return nil
	}
	a := cfg.Attachments
	if a == nil {
		return badf("attachments are not enabled")
	}
	if max := cmp.Or(a.MaxCount, 10); len(atts) > max { // 10: the default, for configs built without Parse
		return badf("at most %d attachments per message", max)
	}
	for _, at := range atts {
		u, err := parseHTTPURL(at.URL)
		if err != nil {
			return badf("attachment %q: %v", at.URL, err)
		}
		if !HostAllowed(a.AllowedHosts, u.Hostname()) {
			return badf("attachment host %q is not allowed", u.Hostname())
		}
		ok := false
		for _, t := range a.Types {
			if strings.EqualFold(t, at.Mime) {
				ok = true
			}
		}
		if !ok {
			return badf("attachment type %q is not allowed", at.Mime)
		}
		if strings.HasPrefix(at.Mime, "image/") && !agent.Settings.Vision {
			return badf("the agent cannot see images (settings.vision is false)")
		}
	}
	return nil
}

// HostAllowed matches a host against exact names and "*.suffix" patterns.
func HostAllowed(patterns []string, host string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if p == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
