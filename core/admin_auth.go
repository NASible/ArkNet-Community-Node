package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"arknet/core/internal/admins"
)

// adminAuthorizer authenticates administrator tokens and stores the audit log.
// Authenticate returns admins.ErrNotFound for anything that is not a live
// token (missing, malformed, unknown, expired or revoked).
type adminAuthorizer interface {
	Authenticate(*http.Request) (admins.Principal, error)
	AppendAudit(context.Context, admins.AuditEntry) error
}

type tokenAuthorizer struct{ repo admins.Repository }

func newTokenAuthorizer(repo admins.Repository) adminAuthorizer { return tokenAuthorizer{repo} }

func (a tokenAuthorizer) Authenticate(r *http.Request) (admins.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return admins.Principal{}, admins.ErrNotFound
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return admins.Principal{}, admins.ErrNotFound
	}
	digest, ok := admins.ParseToken(token)
	if !ok {
		return admins.Principal{}, admins.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	return a.repo.Authenticate(ctx, digest)
}

func (a tokenAuthorizer) AppendAudit(ctx context.Context, entry admins.AuditEntry) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.repo.AppendAudit(ctx, entry)
}

type adminPrincipalKey struct{}

// authenticateAdmin guards the whole admin namespace, including unknown routes:
// without a live token nothing below it runs. It writes no audit row, so
// unauthenticated traffic cannot grow the audit log.
func authenticateAdmin(authorizer adminAuthorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		if authorizer == nil {
			http.NotFound(w, r)
			return
		}
		principal, err := authorizer.Authenticate(r)
		if errors.Is(err, admins.ErrNotFound) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="arknet-core-admin"`)
			adminError(w, http.StatusUnauthorized, "administrative authorization required")
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "administration unavailable")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminPrincipalKey{}, principal)))
	})
}

// adminRoutes registers every admin route with exactly one permission.
type adminRoutes struct {
	mux         *http.ServeMux
	authorizer  adminAuthorizer
	permissions map[string]admins.Permission
}

func newAdminRoutes(mux *http.ServeMux, authorizer adminAuthorizer) *adminRoutes {
	return &adminRoutes{mux: mux, authorizer: authorizer, permissions: map[string]admins.Permission{}}
}

// handle registers pattern behind permission. target names the path value
// recorded as the audit target ("" for none).
func (a *adminRoutes) handle(pattern string, permission admins.Permission, target string, h http.HandlerFunc) {
	if _, ok := admins.ParsePermission(string(permission)); !ok {
		panic("admin route without a valid permission: " + pattern)
	}
	a.permissions[pattern] = permission
	a.mux.HandleFunc(pattern, requireAdminPermission(a.authorizer, pattern, permission, target, h))
}

var auditTargetPattern = regexp.MustCompile(`^[a-z]{1,8}_[0-9a-f]{64}$`)

func auditTarget(r *http.Request, name string) string {
	if name == "" {
		return ""
	}
	if value := r.PathValue(name); auditTargetPattern.MatchString(value) {
		return value
	}
	return "invalid"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(data []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(data)
}

// requireAdminPermission checks the authenticated principal's permission and
// writes the audit log. A permitted action runs only after its "authorized"
// row is stored; a denied one never reaches its handler or any repository.
func requireAdminPermission(authorizer adminAuthorizer, pattern string, permission admins.Permission, target string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := r.Context().Value(adminPrincipalKey{}).(admins.Principal)
		if !ok || authorizer == nil {
			adminError(w, http.StatusUnauthorized, "administrative authorization required")
			return
		}
		entry := admins.AuditEntry{AdminID: principal.AdminID, TokenID: principal.TokenID, Permission: permission,
			Action: pattern, Target: auditTarget(r, target)}
		if !principal.Has(permission) {
			entry.Outcome, entry.Status = admins.OutcomeDenied, http.StatusForbidden
			_ = authorizer.AppendAudit(r.Context(), entry)
			adminError(w, http.StatusForbidden, "administrator token lacks permission "+string(permission))
			return
		}
		entry.Outcome = admins.OutcomeAuthorized
		if err := authorizer.AppendAudit(r.Context(), entry); err != nil {
			adminError(w, http.StatusServiceUnavailable, "administration unavailable")
			return
		}
		recorder := &statusRecorder{ResponseWriter: w}
		h(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		entry.Outcome, entry.Status = admins.OutcomeCompleted, recorder.status
		// Best effort: the action is already attributed by its "authorized" row.
		_ = authorizer.AppendAudit(context.WithoutCancel(r.Context()), entry)
	}
}
