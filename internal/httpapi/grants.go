package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

const (
	defaultGrantTTL = time.Hour
	maxGrantTTL     = 24 * time.Hour
	maxGrantLife    = 7 * 24 * time.Hour // a renewable grant's max_lifetime
	minRenewableTTL = time.Minute        // bounds a chain at maxGrantLife / minRenewableTTL rows
	maxRefLen       = 256
	tokenPrefix     = "hfy_"
)

type createGrantRequest struct {
	Conversation uuid.UUID         `json:"conversation"`
	Actor        string            `json:"actor"`
	Scope        string            `json:"scope"`
	Agent        string            `json:"agent"`
	Vars         map[string]string `json:"vars"`
	ExpiresIn    *int              `json:"expires_in"`   // seconds
	MaxLifetime  *int              `json:"max_lifetime"` // seconds; set: renewable (see exchangeGrant)
}

type createGrantResponse struct {
	ID        uuid.UUID `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// createGrant is server to server: your backend gives one client access to
// one conversation as one actor, and hands it the token. The token is shown
// only here; hfy keeps its hash.
func (s *Server) createGrant(w http.ResponseWriter, r *http.Request) error {
	var req createGrantRequest
	if err := bindJSON(r, &req); err != nil {
		return err
	}
	req.Actor, req.Scope = strings.TrimSpace(types.Clean(req.Actor)), strings.TrimSpace(types.Clean(req.Scope))
	switch {
	case req.Conversation == uuid.UUID{}:
		return &httpError{http.StatusBadRequest, "conversation is required"}
	case req.Actor == "" || len(req.Actor) > maxRefLen || len(req.Scope) > maxRefLen:
		return &httpError{http.StatusBadRequest, "actor is required; actor and scope are at most 256 bytes"}
	}
	if req.Scope == "" {
		req.Scope = req.Actor
	}
	ttl := defaultGrantTTL
	if req.ExpiresIn != nil {
		if *req.ExpiresIn < 1 || *req.ExpiresIn > int(maxGrantTTL/time.Second) { // before converting: n*1e9 overflows
			return &httpError{http.StatusBadRequest, "expires_in must be 1..86400 seconds"}
		}
		ttl = time.Duration(*req.ExpiresIn) * time.Second
	}
	var life time.Duration
	if req.MaxLifetime != nil {
		if *req.MaxLifetime < 1 || *req.MaxLifetime > int(maxGrantLife/time.Second) {
			return &httpError{http.StatusBadRequest, "max_lifetime must be 1..604800 seconds"}
		}
		life = time.Duration(*req.MaxLifetime) * time.Second
		// strictly more: with life == ttl the first token already ends at the cap and could never be exchanged
		if life <= ttl || ttl < minRenewableTTL {
			return &httpError{http.StatusBadRequest, "a renewable grant needs expires_in of at least 60 seconds and max_lifetime greater than expires_in"}
		}
	}
	agent, ok := s.cfg.AgentNamed(req.Agent)
	if !ok {
		return &httpError{http.StatusBadRequest, "unknown agent"}
	}
	if err := svc.CheckVars(agent.Vars, req.Vars); err != nil {
		return err
	}

	ctx := r.Context()
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
		Scope: req.Scope, Agent: req.Agent, Vars: req.Vars, ExpiresIn: ttl, MaxLifetime: life,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, createGrantResponse{ID: id, Token: token, ExpiresAt: expiresAt})
}

type exchangeGrantResponse struct {
	ID        uuid.UUID `json:"id"` // the one the backend stored: revoking it ends this token too
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Renewable bool      `json:"renewable"` // false at max_lifetime: the next token has to come from the backend
}

// exchangeGrant swaps a renewable grant's token for a fresh one, the same
// identity with a new lifetime, capped at the grant's max_lifetime. The old
// token works for exchangeGrace more seconds (requests in flight, a second tab)
// but cannot be exchanged again. A live grant that is not renewable, or has
// reached its cap, is 409; one asking before a quarter of its lifetime has
// passed is 429 with the seconds to wait; any other token is the same 401 as
// everywhere, so nothing is learned about dead ones.
//
// shortcut: a retired token presented after its exchange is simply refused;
// the rows would allow telling a replay (theft) from a lost response, and
// revoking the chain on it, when that is wanted.
func (s *Server) exchangeGrant(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	ctx := r.Context()
	old := hashToken(bearer(r))
	token, hash := newToken()
	got, err := s.store.ExchangeGrant(ctx, old, hash, exchangeGrace, exchangeGap)
	switch {
	case db.IsNotFound(err) && !g.Renewable:
		return errNotRenewable
	case db.IsNotFound(err):
		miss, merr := s.store.ExchangeMiss(ctx, old, exchangeGap)
		if merr != nil && !db.IsNotFound(merr) {
			return merr
		}
		switch code, wait := exchangeRefusal(miss, db.IsNotFound(merr)); code {
		case http.StatusConflict:
			return errNotRenewable
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			return writeJSON(w, code, tooEarlyBody{Error: "too early to renew", RetryAfter: wait}) // the body too: a page cannot read the header cross-origin
		}
		return &httpError{http.StatusUnauthorized, "invalid or expired token"}
	case err != nil:
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, exchangeGrantResponse{ID: got.ChainID, Token: token, ExpiresAt: got.ExpiresAt, Renewable: got.Renewable})
}

// exchangeRefusal answers a live renewable token that was not exchanged. Dead
// and retired tokens are the usual 401 and a capped one is a 409. Anything else
// is too early, and always says to wait at least a second: the exchange can
// miss by a few milliseconds and the lookup after it then finds nothing left
// to wait, which must not read as a dead token (the client would ask the host).
func exchangeRefusal(m db.ExchangeMissed, notLive bool) (code, wait int) {
	switch {
	case notLive || m.Retired:
		return http.StatusUnauthorized, 0
	case m.Capped:
		return http.StatusConflict, 0
	}
	return http.StatusTooManyRequests, max(m.Wait, 1)
}

var errNotRenewable = &httpError{http.StatusConflict, "grant is not renewable"}

type tooEarlyBody struct {
	Error      string `json:"error"`
	RetryAfter int    `json:"retry_after"` // seconds
}

// exchangeGrace is how long a retired token keeps working, and exchangeGap the
// share of a grant's lifetime that must pass before it is exchanged (the
// client asks at half or later). Variables only so tests can change them;
// they are not settings.
var (
	exchangeGrace = 30 * time.Second
	exchangeGap   = 0.25
)

// revokeGrant ends a grant now, and every token it was exchanged for, open
// streams included.
func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return &httpError{http.StatusNotFound, "not found"}
	}
	if err := s.store.RevokeGrant(r.Context(), id); err != nil {
		return err // unknown: 404
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
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
