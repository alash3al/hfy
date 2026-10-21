// Package httpapi is the HTTP surface: Echo routes, the two auth middlewares
// and SSE. One file per action, each holding its own DTOs.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/auth"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

type Server struct {
	cfg   *config.Config
	store *db.Store
	svc   *svc.Service
	hub   *Hub
	auth  *auth.Client // nil = no auth
	echo  *echo.Echo
}

func New(cfg *config.Config, store *db.Store, s *svc.Service, hub *Hub) *Server {
	srv := &Server{cfg: cfg, store: store, svc: s, hub: hub}
	if cfg.Auth != nil {
		srv.auth = auth.New(cfg.Auth.Webhook, cfg.Auth.Timeout.D())
	}
	e := echo.New()
	e.HTTPErrorHandler = srv.handleError
	e.Use(observe, limitBody)
	srv.echo = e

	// Operational endpoints are unversioned; the API is under /v1.
	e.GET("/healthz", echo.WrapHandler(http.HandlerFunc(Healthz)))
	e.GET("/readyz", echo.WrapHandler(Readyz(store)))

	v1 := e.Group("/v1")
	server := v1.Group("", srv.requireSecret)
	server.POST("/conversations", srv.createConversation)

	client := v1.Group("/conversations/:id", srv.requireClient)
	client.POST("/messages", srv.postMessage)
	client.GET("/events", srv.streamEvents)
	client.GET("/history", srv.history)
	client.POST("/approvals", srv.approve)
	client.POST("/stop", srv.stop)
	return srv
}

func (s *Server) Handler() http.Handler { return s.echo }

// Healthz is liveness: the process serves requests. It never checks
// dependencies, so a database outage does not restart every instance.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, "ok")
}

// Readyz is readiness: this instance can reach Postgres.
func Readyz(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Pool.Ping(ctx); err != nil {
			writeStatus(w, http.StatusServiceUnavailable, "database unreachable")
			return
		}
		writeStatus(w, http.StatusOK, "ok")
	}
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status}) // the client is gone; nothing to do
}

// observe records request counts and latency by route pattern (never the raw
// path, which would be one series per conversation).
func observe(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		start := time.Now()
		err := next(c)
		if err != nil {
			c.Echo().HTTPErrorHandler(c, err) // settles the status code
		}
		_, code := echo.ResolveResponseStatus(c.Response(), err)
		route, method := c.Path(), c.Request().Method
		if route == "" {
			route = "unmatched"
		}
		metrics.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
		if !strings.HasSuffix(route, "/events") { // streams last minutes; not latency
			metrics.Since(metrics.HTTPDuration.WithLabelValues(route, method), start)
		}
		return err
	}
}

// maxBody bounds every request body (a message with its attachment
// references is the largest), so no client makes the server allocate freely.
const maxBody = 2 << 20

func limitBody(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxBody)
		return next(c)
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func (s *Server) handleError(c *echo.Context, err error) {
	if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
		return
	}
	code, msg := http.StatusInternalServerError, "internal error"
	he, isHTTP := errors.AsType[*echo.HTTPError](err)
	var sc echo.HTTPStatusCoder // Echo's own errors (404, 405) are a private type; not an error type, so no AsType
	hasCode := errors.As(err, &sc)
	bad, isBad := errors.AsType[*svc.BadRequestError](err)
	switch {
	case isHTTP:
		code, msg = he.Code, he.Message
	case hasCode:
		code, msg = sc.StatusCode(), strings.ToLower(http.StatusText(sc.StatusCode()))
	case isBad:
		code, msg = http.StatusBadRequest, bad.Msg
	case errors.Is(err, svc.ErrNoPendingApproval):
		code, msg = http.StatusConflict, err.Error()
	case errors.Is(err, svc.ErrNotTurnOwner):
		code, msg = http.StatusForbidden, err.Error()
	case db.IsNotFound(err):
		code, msg = http.StatusNotFound, "not found"
	default:
		slog.Error("request failed", "method", c.Request().Method, "path", c.Request().URL.Path, "err", err)
	}
	_ = c.JSON(code, errorBody{Error: msg})
}

func bearer(c *echo.Context) string {
	if h := c.Request().Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

const maxHint = 4 << 10

// hintOf is the client's Hfy-Hint header: any JSON value for the auth webhook
// (say, which agent to ask), forwarded unread and never stored.
func hintOf(c *echo.Context) (json.RawMessage, error) {
	h := c.Request().Header.Get("Hfy-Hint")
	if h == "" {
		return nil, nil
	}
	if len(h) > maxHint || !json.Valid([]byte(h)) {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "Hfy-Hint must be JSON of at most 4 KiB")
	}
	return json.RawMessage(h), nil
}

// requireSecret guards the server-to-server endpoints. Hashing first makes the
// comparison constant-time regardless of the secret's length.
func (s *Server) requireSecret(next echo.HandlerFunc) echo.HandlerFunc {
	want := sha256.Sum256([]byte(s.cfg.Server.Secret))
	return func(c *echo.Context) error {
		got := sha256.Sum256([]byte(bearer(c)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid server secret")
		}
		return next(c)
	}
}

// caller is who is acting on a client request.
type caller struct {
	actorID      int64
	scope        string
	vars         map[string]string
	agent        string // the configured agent serving this caller ("" = main)
	conversation db.ConversationRow
}

const callerKey = "hfy.caller"

// requireClient resolves the conversation and asks the webhook (if configured)
// whether the key may use it. The webhook alone decides access.
func (s *Server) requireClient(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		cid, err := uuid.Parse(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		ctx := c.Request().Context()
		actorType, ref, scope := types.ActorAnonymous, types.NoScope, types.NoScope
		var vars map[string]string
		agent := ""
		if s.auth != nil {
			key := bearer(c)
			if key == "" {
				key = c.QueryParam("key") // EventSource cannot set headers
			}
			if key == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing key")
			}
			hint, err := hintOf(c)
			if err != nil {
				return err
			}
			id, err := s.auth.Check(ctx, key, cid.String(), hint)
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
			case err != nil:
				return echo.NewHTTPError(http.StatusForbidden, "forbidden")
			}
			if _, ok := s.cfg.AgentNamed(id.Agent); !ok {
				slog.Error("auth webhook picked an unknown agent", "agent", id.Agent)
				return echo.NewHTTPError(http.StatusForbidden, "forbidden") // fail closed
			}
			actorType, ref, scope, vars, agent = types.ActorExternal, id.Actor, id.Scope, id.Vars, id.Agent
		}
		conv, err := s.store.ConversationByUUID(ctx, cid)
		if err != nil {
			return err
		}
		actor, err := s.store.FindOrCreateActor(ctx, s.store.Pool, actorType, ref)
		if err != nil {
			return err
		}
		c.Set(callerKey, &caller{actorID: actor, scope: scope, vars: vars, agent: agent, conversation: conv})
		return next(c)
	}
}

func callerOf(c *echo.Context) *caller { return c.Get(callerKey).(*caller) }
