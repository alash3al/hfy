// Package httpapi is the HTTP surface: Echo routes, the two auth middlewares
// and SSE. One file per action, each holding its own DTOs.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"uuid"

	"github.com/labstack/echo/v5"

	"github.com/alash3al/hfy/internal/auth"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
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
	srv.echo = e

	e.GET("/healthz", func(c *echo.Context) error { return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) })

	server := e.Group("", srv.requireSecret)
	server.POST("/sessions", srv.createSession)

	client := e.Group("/sessions/:id", srv.requireClient)
	client.POST("/messages", srv.postMessage)
	client.GET("/events", srv.streamEvents)
	client.GET("/history", srv.history)
	client.POST("/approvals", srv.approve)
	client.POST("/stop", srv.stop)
	return srv
}

func (s *Server) Handler() http.Handler { return s.echo }

type errorBody struct {
	Error string `json:"error"`
}

func (s *Server) handleError(c *echo.Context, err error) {
	if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
		return
	}
	code, msg := http.StatusInternalServerError, "internal error"
	var he *echo.HTTPError
	var bad *svc.BadRequestError
	switch {
	case errors.As(err, &he):
		code, msg = he.Code, he.Message
	case errors.As(err, &bad):
		code, msg = http.StatusBadRequest, bad.Msg
	case errors.Is(err, svc.ErrNoPendingApproval):
		code, msg = http.StatusConflict, err.Error()
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
	actorID int64
	scope   string
	vars    map[string]string
	session db.SessionRow
}

const callerKey = "hfy.caller"

// requireClient resolves the session and asks the webhook (if configured)
// whether the key may use it. The webhook alone decides access.
func (s *Server) requireClient(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		sid, err := uuid.Parse(c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		ctx := c.Request().Context()
		actorType, ref, scope := types.ActorAnonymous, types.NoScope, types.NoScope
		var vars map[string]string
		if s.auth != nil {
			key := bearer(c)
			if key == "" {
				key = c.QueryParam("key") // EventSource cannot set headers
			}
			if key == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing key")
			}
			id, err := s.auth.Check(ctx, key, sid.String())
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
			case err != nil:
				return echo.NewHTTPError(http.StatusForbidden, "forbidden")
			}
			actorType, ref, scope, vars = types.ActorExternal, id.Actor, id.Scope, id.Vars
		}
		sess, err := s.store.SessionByUUID(ctx, sid)
		if err != nil {
			return err
		}
		actor, err := s.store.FindOrCreateActor(ctx, actorType, ref)
		if err != nil {
			return err
		}
		c.Set(callerKey, &caller{actorID: actor, scope: scope, vars: vars, session: sess})
		return next(c)
	}
}

func callerOf(c *echo.Context) *caller { return c.Get(callerKey).(*caller) }
