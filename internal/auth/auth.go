// Package auth calls the client auth webhook. hfy has no auth logic of its own:
// the key is never interpreted, stored or logged, only forwarded.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ErrDenied = errors.New("access denied")

// ErrUnauthorized is a webhook 401: the key itself was not accepted.
var ErrUnauthorized = errors.New("unauthorized")

// Identity is what the webhook says about the caller.
type Identity struct {
	Actor string
	Scope string // defaults to Actor
	Vars  map[string]string
	Agent string // which configured agent serves this caller ("" = main)
}

type Client struct {
	url  string
	http *http.Client
}

func New(url string, timeout time.Duration) *Client {
	return &Client{url: url, http: &http.Client{Timeout: timeout}}
}

// Check asks the webhook whether key may use the session. Anything but a clean
// 200 with an actor denies (fail closed).
func (c *Client) Check(ctx context.Context, key, sessionID string) (Identity, error) {
	body, _ := json.Marshal(map[string]string{"key": key, "session_id": sessionID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Identity{}, ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: webhook unreachable", ErrDenied)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Identity{}, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return Identity{}, ErrDenied
	}
	var out struct {
		Actor string         `json:"actor"`
		Scope string         `json:"scope"`
		Vars  map[string]any `json:"vars"`
		Agent string         `json:"agent"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Actor == "" {
		return Identity{}, fmt.Errorf("%w: bad webhook response", ErrDenied)
	}
	id := Identity{Actor: out.Actor, Scope: out.Scope, Vars: map[string]string{}, Agent: out.Agent}
	if id.Scope == "" {
		id.Scope = id.Actor
	}
	for k, v := range out.Vars {
		switch t := v.(type) {
		case string:
			id.Vars[k] = t
		default:
			b, _ := json.Marshal(t)
			id.Vars[k] = string(b)
		}
	}
	return id, nil
}
