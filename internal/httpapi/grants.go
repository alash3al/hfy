package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

const (
	defaultGrantTTL = time.Hour
	maxGrantTTL     = 24 * time.Hour
	maxRefLen       = 256
	tokenPrefix     = "hfy_"
)

type createGrantRequest struct {
	Conversation uuid.UUID         `json:"conversation"`
	Actor        string            `json:"actor"`
	Scope        string            `json:"scope"`
	Agent        string            `json:"agent"`
	Vars         map[string]string `json:"vars"`
	ExpiresIn    *int              `json:"expires_in"` // seconds
}

type createGrantResponse struct {
	ID        uuid.UUID `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// createGrant is server to server: your backend gives one client access to
// one conversation as one actor, and hands it the token. The token is shown
// only here; hfy keeps its hash.
func (s *Server) createGrant(c *echo.Context) error {
	var req createGrantRequest
	if err := bindJSON(c, &req); err != nil {
		return err
	}
	req.Actor, req.Scope = strings.TrimSpace(types.Clean(req.Actor)), strings.TrimSpace(types.Clean(req.Scope))
	switch {
	case req.Conversation == uuid.UUID{}:
		return echo.NewHTTPError(http.StatusBadRequest, "conversation is required")
	case req.Actor == "" || len(req.Actor) > maxRefLen || len(req.Scope) > maxRefLen:
		return echo.NewHTTPError(http.StatusBadRequest, "actor is required; actor and scope are at most 256 bytes")
	}
	if req.Scope == "" {
		req.Scope = req.Actor
	}
	ttl := defaultGrantTTL
	if req.ExpiresIn != nil {
		ttl = time.Duration(*req.ExpiresIn) * time.Second
		if ttl < time.Second || ttl > maxGrantTTL {
			return echo.NewHTTPError(http.StatusBadRequest, "expires_in must be 1..86400 seconds")
		}
	}
	agent, ok := s.cfg.AgentNamed(req.Agent)
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "unknown agent")
	}
	if err := svc.CheckVars(agent.Vars, req.Vars); err != nil {
		return err
	}

	ctx := c.Request().Context()
	conv, err := s.store.ConversationByUUID(ctx, req.Conversation)
	if err != nil {
		return err // unknown: 404
	}
	actor, err := s.store.FindOrCreateActor(ctx, s.store.Pool, types.ActorExternal, req.Actor)
	if err != nil {
		return err
	}
	token, hash := newToken()
	id, expiresAt, err := s.store.CreateGrant(ctx, db.NewGrant{
		TokenHash: hash, ConversationID: conv.ID, ActorID: actor,
		Scope: req.Scope, Agent: req.Agent, Vars: req.Vars, ExpiresIn: ttl,
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, createGrantResponse{ID: id, Token: token, ExpiresAt: expiresAt})
}

// revokeGrant ends a grant now, open streams included.
func (s *Server) revokeGrant(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := s.store.RevokeGrant(c.Request().Context(), id); err != nil {
		return err // unknown: 404
	}
	return c.NoContent(http.StatusNoContent)
}

// newToken returns a fresh token and the hash hfy stores. 256 random bits
// cannot be guessed, so a fast hash is enough: nothing in the database works
// as a token.
func newToken() (token string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
