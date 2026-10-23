package svc

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/types"
)

// BadRequestError is a client mistake (HTTP 400).
type BadRequestError struct{ Msg string }

func (e *BadRequestError) Error() string { return e.Msg }

func badf(format string, a ...any) error { return &BadRequestError{fmt.Sprintf(format, a...)} }

// CheckVars is what a grant may carry: only vars the agent declares, and
// every required one. Its mistakes are the backend's, so they are a 400 to
// the backend when it creates the grant.
func CheckVars(decl map[string]config.Var, given map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(given)) {
		if _, ok := decl[name]; !ok {
			return badf("vars.%s is not declared by the agent", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(decl)) {
		if _, ok := given[name]; decl[name].Required && !ok {
			return badf("vars.%s is required", name)
		}
	}
	return nil
}

// ResolveVars fills the declared vars from a grant's: vars render into the
// system prompt, so only the app's backend sets them, never a client. Defaults
// fill the gaps and vars the agent no longer declares are dropped. A required
// var the grant lacks means the config changed after the grant was created
// (a server error, not the client's).
func ResolveVars(decl map[string]config.Var, granted map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for name, d := range decl {
		v, ok := granted[name]
		switch {
		case ok:
			out[name] = v
		case d.Required:
			return nil, fmt.Errorf("the grant lacks the required var %q (the agent's config changed since)", name)
		case d.Default != nil:
			out[name] = *d.Default
		default:
			out[name] = ""
		}
	}
	return out, nil
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
	if len(atts) > a.MaxCount {
		return badf("at most %d attachments per message", a.MaxCount)
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
