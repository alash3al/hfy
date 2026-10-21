// Package httpapi is the HTTP surface: Echo routes, the two auth middlewares
// (the server secret, a grant's token) and SSE. One file per action, each holding its own DTOs.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/svc"
)

type Server struct {
	cfg   *config.Config
	store *db.Store
	svc   *svc.Service
	hub   *Hub
	echo  *echo.Echo
}

func New(cfg *config.Config, store *db.Store, s *svc.Service, hub *Hub) *Server {
	srv := &Server{cfg: cfg, store: store, svc: s, hub: hub}
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
	server.POST("/grants", srv.createGrant)
	server.DELETE("/grants/:id", srv.revokeGrant)

	// A client's routes name no conversation: its grant holds it. Only the
	// stream takes the token as ?token=, since EventSource cannot set headers.
	client := v1.Group("/conversation")
	if origins := cfg.Server.CORSOrigins; len(origins) > 0 {
		// Browsers may call these routes only. Tokens travel in a header, never
		// in cookies, so no credentials mode; preflights are cached an hour.
		client.Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: origins,
			AllowMethods: []string{http.MethodGet, http.MethodPost},
			AllowHeaders: []string{"Authorization", "Content-Type", "Last-Event-ID"},
			MaxAge:       3600,
		}))
	}
	header, headerOrQuery := srv.requireGrant(false), srv.requireGrant(true)
	client.POST("/messages", srv.postMessage, header)
	client.GET("/events", srv.streamEvents, headerOrQuery)
	client.GET("/history", srv.history, header)
	client.POST("/approvals", srv.approve, header)
	client.POST("/stop", srv.stop, header)
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

// bindJSON decodes a request body as JSON whatever its Content-Type says:
// the API speaks only JSON, and `curl -d` labels it a form. An empty body is
// an empty object.
func bindJSON(c *echo.Context, v any) error {
	err := json.NewDecoder(c.Request().Body).Decode(v)
	if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "body over 2 MiB")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid json body")
	}
	return nil
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
		attrs := []any{"method", c.Request().Method, "path", c.Request().URL.Path, "err", err}
		if g, ok := c.Get(grantKey).(*db.GrantRow); ok { // client URLs name no conversation
			attrs = append(attrs, "conversation", g.ConversationUUID)
		}
		slog.Error("request failed", attrs...)
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

const grantKey = "hfy.grant"

// requireGrant resolves the request's token to its grant: who acts, in which
// conversation, as which agent, with which vars. A token that is unknown,
// expired or revoked gets one answer, so a client learns nothing about which.
func (s *Server) requireGrant(queryToken bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			token := bearer(c)
			if token == "" && queryToken {
				token = c.QueryParam("token")
			}
			if token == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing token")
			}
			g, err := s.store.GrantByToken(c.Request().Context(), hashToken(token))
			if db.IsNotFound(err) {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired token")
			}
			if err != nil {
				return err
			}
			if _, ok := s.cfg.AgentNamed(g.Agent); !ok {
				slog.Error("a grant names an agent the config no longer has", "agent", g.Agent, "grant", g.ID)
				return echo.NewHTTPError(http.StatusForbidden, "forbidden") // fail closed
			}
			c.Set(grantKey, &g)
			return next(c)
		}
	}
}

func grantOf(c *echo.Context) *db.GrantRow { return c.Get(grantKey).(*db.GrantRow) }
