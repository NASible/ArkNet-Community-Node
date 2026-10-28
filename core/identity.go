package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"arknet/core/internal/users"
)

// MVP account mechanism: administrators create global users through the
// protected administrative API. There is no open self-registration endpoint.
// Login issues an opaque bearer token with an absolute users.SessionTTL.

const identityBodyLimit = 4096

// invalidCredentials is the single response for unknown users, wrong
// passwords, malformed login input and disabled users, so a caller cannot
// enumerate accounts or learn account status.
func invalidCredentials(w http.ResponseWriter) {
	writeAdminJSON(w, http.StatusUnauthorized, struct {
		Error string `json:"error"`
	}{"invalid credentials"})
}

func readCredentialsBody(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "", "", false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, identityBodyLimit))
	if err != nil {
		return "", "", false
	}
	fields, ok := exactObject(body, "username", "password")
	if !ok {
		return "", "", false
	}
	var username, password string
	if json.Unmarshal(fields["username"], &username) != nil || json.Unmarshal(fields["password"], &password) != nil {
		return "", "", false
	}
	return username, password, true
}

func registerIdentityRoutes(mux *http.ServeMux, repo users.Repository) {
	mux.HandleFunc("POST /api/identity/v1/sessions", loginHandler(repo))
	mux.HandleFunc("GET /api/identity/v1/session", currentSessionHandler(repo))
	mux.HandleFunc("DELETE /api/identity/v1/session", logoutHandler(repo))
	mux.HandleFunc("POST /api/identity/v1/password", passwordChangeHandler(repo))
}

// passwordChangeHandler lets a signed-in user replace their own password. It
// requires the current password, answers a wrong one exactly like a failed
// login, and on success revokes every session of the user, including this one.
func passwordChangeHandler(repo users.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digest, ok := bearerDigest(r)
		if !ok {
			sessionRequired(w)
			return
		}
		var current, replacement string
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, identityBodyLimit))
		fields, exact := exactObject(body, "current_password", "new_password")
		if err != nil || !exact || r.URL.RawQuery != "" || r.URL.ForceQuery ||
			json.Unmarshal(fields["current_password"], &current) != nil ||
			json.Unmarshal(fields["new_password"], &replacement) != nil {
			adminError(w, http.StatusBadRequest, "password change requires exactly current_password and new_password strings")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		session, err := repo.GetSession(ctx, digest)
		if errors.Is(err, users.ErrNotFound) {
			sessionRequired(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		if users.ValidatePassword(replacement) != nil || replacement == current {
			adminError(w, http.StatusBadRequest, "new password must differ from the current one and meet the length policy")
			return
		}
		credentials, err := repo.GetCredentialsByID(ctx, session.UserID)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		matched, err := users.VerifyPassword(ctx, credentials.PasswordHash, current)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		if !matched || credentials.Status != users.StatusActive {
			invalidCredentials(w)
			return
		}
		hash, err := users.HashPassword(ctx, replacement)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		err = repo.ReplacePassword(ctx, session.UserID, credentials.PasswordHash, hash)
		if errors.Is(err, users.ErrNotFound) || errors.Is(err, users.ErrInactive) {
			// Lost a race with another change or a disable: the caller must retry.
			invalidCredentials(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func loginHandler(repo users.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		rawUsername, password, ok := readCredentialsBody(w, r)
		if !ok {
			adminError(w, http.StatusBadRequest, "login requires exactly username and password strings")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		username, usernameErr := users.NormalizeUsername(rawUsername)
		if usernameErr != nil || users.ValidatePassword(password) != nil {
			if users.VerifyDummy(ctx, password) != nil {
				adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
				return
			}
			invalidCredentials(w)
			return
		}
		credentials, err := repo.GetCredentials(ctx, username)
		if errors.Is(err, users.ErrNotFound) {
			if users.VerifyDummy(ctx, password) != nil {
				adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
				return
			}
			invalidCredentials(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		matched, err := users.VerifyPassword(ctx, credentials.PasswordHash, password)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		// Status is checked only after full verification so timing does not reveal it.
		if !matched || credentials.Status != users.StatusActive {
			invalidCredentials(w)
			return
		}
		token, digest, err := users.NewSessionToken()
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		expiresAt, err := repo.CreateSession(ctx, credentials.UserID, digest, users.SessionTTL)
		if errors.Is(err, users.ErrInactive) {
			invalidCredentials(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		writeAdminJSON(w, http.StatusCreated, struct {
			UserID    string `json:"user_id"`
			Token     string `json:"token"`
			TokenType string `json:"token_type"`
			ExpiresAt string `json:"expires_at"`
		}{credentials.UserID, token, "Bearer", expiresAt.UTC().Format(time.RFC3339)})
	}
}

// bearerDigest accepts exactly one canonical "Bearer <token>" header.
func bearerDigest(r *http.Request) ([32]byte, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return [32]byte{}, false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return [32]byte{}, false
	}
	return users.SessionDigest(token)
}

func sessionRequired(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="arknet-identity"`)
	adminError(w, http.StatusUnauthorized, "valid session required")
}

func currentSessionHandler(repo users.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		digest, ok := bearerDigest(r)
		if !ok || r.URL.RawQuery != "" {
			sessionRequired(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := repo.GetSession(ctx, digest)
		if errors.Is(err, users.ErrNotFound) {
			sessionRequired(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		writeAdminJSON(w, http.StatusOK, struct {
			UserID    string `json:"user_id"`
			ExpiresAt string `json:"expires_at"`
		}{session.UserID, session.ExpiresAt.UTC().Format(time.RFC3339)})
	}
}

func logoutHandler(repo users.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		digest, ok := bearerDigest(r)
		if !ok {
			sessionRequired(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// Idempotent: deleting an unknown or expired digest reveals nothing.
		if err := repo.DeleteSession(ctx, digest); err != nil {
			adminError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type administrativeUser struct {
	UserID    string       `json:"user_id"`
	Username  string       `json:"username"`
	Status    users.Status `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
}

// registerUserAdministration adds routes to the protected admin mux only.
// There is deliberately no user listing: the global directory is not browsable.
func registerUserAdministration(admin *http.ServeMux, repo users.Repository) {
	admin.HandleFunc("POST /api/admin/v1/users", func(w http.ResponseWriter, r *http.Request) {
		rawUsername, password, ok := readCredentialsBody(w, r)
		if !ok {
			adminError(w, http.StatusBadRequest, "user creation requires exactly username and password strings")
			return
		}
		username, err := users.NormalizeUsername(rawUsername)
		if err != nil {
			adminError(w, http.StatusBadRequest, "username must be 3-32 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
			return
		}
		if users.ValidatePassword(password) != nil {
			adminError(w, http.StatusBadRequest, "password must be 12-256 bytes of valid UTF-8")
			return
		}
		if repo == nil {
			adminError(w, http.StatusServiceUnavailable, "administration unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		hash, err := users.HashPassword(ctx, password)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "administration unavailable")
			return
		}
		user, err := repo.CreateUser(ctx, username, hash)
		if errors.Is(err, users.ErrUsernameTaken) {
			adminError(w, http.StatusConflict, "username is already registered")
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "administration unavailable")
			return
		}
		writeAdminJSON(w, http.StatusCreated, administrativeUser{user.ID, user.Username, user.Status, user.CreatedAt})
	})
	for action, status := range map[string]users.Status{"disable": users.StatusDisabled, "enable": users.StatusActive} {
		admin.HandleFunc("POST /api/admin/v1/users/{user_id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("user_id")
			if !users.ValidUserID(id) {
				adminError(w, http.StatusBadRequest, "invalid user ID")
				return
			}
			if r.URL.RawQuery != "" || r.URL.ForceQuery {
				adminError(w, http.StatusBadRequest, "action does not accept parameters")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
			if err != nil || len(body) != 0 {
				adminError(w, http.StatusBadRequest, "action requires an empty body")
				return
			}
			if repo == nil {
				adminError(w, http.StatusServiceUnavailable, "administration unavailable")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			user, err := repo.SetUserStatus(ctx, id, status)
			if errors.Is(err, users.ErrNotFound) {
				adminError(w, http.StatusNotFound, "user not found")
				return
			}
			if err != nil {
				adminError(w, http.StatusServiceUnavailable, "administration unavailable")
				return
			}
			writeAdminJSON(w, http.StatusOK, administrativeUser{user.ID, user.Username, user.Status, user.CreatedAt})
		})
	}
}
