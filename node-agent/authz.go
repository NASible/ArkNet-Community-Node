package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"arknet/node-agent/internal/operators"
)

// Node management-plane authorization (Prompt 45B, ARKSEC-002). One
// middleware enforces routePermissions before any handler runs, so a denied
// request never reaches a repository, Core, Jellyfin or the transfer service.
// See the permission matrix in docs/node-operator-authentication.md.

type permission string

const (
	// permPublic routes need no credential (health and member/operator login).
	permPublic permission = "public"
	// permMember routes check the caller's own member bearer session inside the
	// handler; they never confer operator rights.
	permMember   permission = "member"
	permOperator permission = "operator"
	// permStepUp additionally requires a sign-in within operators.StepUpWindow.
	permStepUp permission = "operator+step-up"
)

// routePermissions is the code form of the design's permission matrix. Every
// registered pattern must appear here (TestEveryRegisteredRouteHasAPermission);
// an unlisted pattern is denied at runtime.
var routePermissions = map[string]permission{
	"GET /health":                               permPublic,
	"GET /api/v1/status":                        permOperator,
	"GET /api/v1/diagnostics":                   permOperator,
	"GET /api/v1/media":                         permOperator,
	"POST /api/v1/media/{id}/selection":         permOperator,
	"DELETE /api/v1/media/{id}/retained":        permOperator,
	"GET /api/v1/transfers":                     permOperator,
	"POST /api/v1/media/{id}/transfer/{action}": permOperator,
	"POST /api/v1/invitations":                  permStepUp,
	"GET /api/v1/settings/peer-serving":         permOperator,
	"PUT /api/v1/settings/peer-serving":         permOperator,
	"GET /api/v1/settings/media-miss-policy":    permOperator,
	"PUT /api/v1/settings/media-miss-policy":    permOperator,
	"GET /api/v1/media-misses":                  permOperator,
	"POST /api/v1/media-misses/{id}/ignore":     permOperator,
	"GET /api/v1/updates":                       permOperator,
	"POST /api/v1/updates/check":                permOperator,
	"GET /api/v1/settings/automatic-updates":    permOperator,
	"PUT /api/v1/settings/automatic-updates":    permOperator,
	"GET /api/v1/local-users":                   permOperator,
	"POST /api/v1/local-users":                  permOperator,
	"POST /api/v1/local-users/{id}/disable":     permOperator,
	"POST /api/v1/local-users/{id}/enable":      permOperator,
	"POST /api/v1/local-users/sessions":         permPublic,
	"GET /api/v1/local-users/session":           permMember,
	"DELETE /api/v1/local-users/session":        permMember,
	"POST /api/v1/global-users/sessions":        permPublic,
	"POST /api/v1/global-users/login":           permPublic,
	"POST /api/v1/global-users/login/complete":  permPublic,
	"GET /api/v1/global-users/session":          permMember,
	"DELETE /api/v1/global-users/session":       permMember,
	"POST /api/v1/operator/setup":               permPublic,
	"POST /api/v1/operator/setup/confirm":       permPublic,
	"POST /api/v1/operator/sessions":            permPublic,
	"GET /api/v1/operator/session":              permOperator,
	"DELETE /api/v1/operator/session":           permOperator,
}

// routeMux records every registered pattern so tests can check the matrix
// against what is actually served.
type routeMux struct {
	mux      *http.ServeMux
	patterns []string
}

func newRouteMux() *routeMux { return &routeMux{mux: http.NewServeMux()} }

func (m *routeMux) Handle(pattern string, handler http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.mux.Handle(pattern, handler)
}

func (m *routeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

func (m *routeMux) Handler(r *http.Request) (http.Handler, string)   { return m.mux.Handler(r) }
func (m *routeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

type listenerPartition struct {
	routes  *routeMux
	next    http.Handler
	allowed func(permission) bool
}

func (p listenerPartition) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := p.routes.Handler(r)
	permission, listed := routePermissions[pattern]
	if pattern == "" || !listed || !p.allowed(permission) {
		http.NotFound(w, r)
		return
	}
	p.next.ServeHTTP(w, r)
}

func memberPermission(p permission) bool {
	return p == permPublic || p == permMember
}

func managementPermission(p permission) bool {
	return p == permOperator || p == permStepUp
}

type operatorContextKey struct{}

// operatorFromContext returns the authorized operator session, if any.
func operatorFromContext(ctx context.Context) (operators.Session, bool) {
	session, ok := ctx.Value(operatorContextKey{}).(operators.Session)
	return session, ok
}

func authorize(mux *routeMux, sessions operators.Repository, now func() time.Time, logger *slog.Logger) http.Handler {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	deny := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("WWW-Authenticate", `Cookie realm="arknet-operator"`)
		mediaError(w, status, code)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			mux.ServeHTTP(w, r) // the mux's own 404/405; no ArkNet handler runs
			return
		}
		perm, listed := routePermissions[pattern]
		if !listed {
			logger.Error("route_without_permission", "pattern", pattern)
			mediaError(w, http.StatusForbidden, "operator_required")
			return
		}
		if perm == permPublic || perm == permMember {
			mux.ServeHTTP(w, r)
			return
		}
		digest, ok := operatorSessionDigest(r)
		if !ok {
			deny(w, http.StatusUnauthorized, "operator_required")
			return
		}
		if sessions == nil {
			mediaError(w, http.StatusServiceUnavailable, "operators_unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := sessions.GetOperatorSession(ctx, digest)
		if errors.Is(err, operators.ErrNotFound) {
			clearOperatorCookie(w)
			deny(w, http.StatusUnauthorized, "operator_required")
			return
		}
		if err != nil {
			mediaError(w, http.StatusServiceUnavailable, "operators_unavailable")
			return
		}
		if perm == permStepUp && now().Sub(session.AuthenticatedAt) > operators.StepUpWindow {
			deny(w, http.StatusUnauthorized, "reauthentication_required")
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operatorContextKey{}, session)))
	})
}
