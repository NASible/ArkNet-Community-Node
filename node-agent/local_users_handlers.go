package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/localusers"
)

// localUsersOpt serves Node-local accounts entirely from local storage.
type localUsersOpt struct {
	repository localusers.Repository
}

func (localUsersOpt) isOption() {}

type localUserView struct {
	UserID    string            `json:"user_id"`
	Username  string            `json:"username"`
	Kind      string            `json:"kind"`
	Status    localusers.Status `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
}

func localView(user localusers.User) localUserView {
	return localUserView{user.ID, user.Username, localusers.Kind, user.Status, user.CreatedAt}
}

func localCredentialsBody(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" {
		return "", "", false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
	}
	if err != nil || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Username == nil || value.Password == nil {
		return "", "", false
	}
	return *value.Username, *value.Password, true
}

func localInvalidCredentials(w http.ResponseWriter) {
	mediaError(w, http.StatusUnauthorized, "invalid_credentials")
}

func localSessionDigest(r *http.Request) ([32]byte, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return [32]byte{}, false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return [32]byte{}, false
	}
	return localusers.SessionDigest(token)
}

func registerLocalUsers(mux *routeMux, local localUsersOpt) {
	repo := local.repository
	protect := func(handler http.HandlerFunc) http.Handler {
		return http.NewCrossOriginProtection().Handler(handler)
	}
	unavailable := func(w http.ResponseWriter) { mediaError(w, http.StatusServiceUnavailable, "local_users_unavailable") }

	mux.HandleFunc("GET /api/v1/local-users", func(w http.ResponseWriter, r *http.Request) {
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		users, err := repo.ListLocalUsers(ctx, localusers.MaxListedUsers)
		if err != nil {
			unavailable(w)
			return
		}
		truncated := len(users) > localusers.MaxListedUsers
		if truncated {
			users = users[:localusers.MaxListedUsers]
		}
		views := make([]localUserView, 0, len(users))
		for _, user := range users {
			views = append(views, localView(user))
		}
		mediaJSON(w, http.StatusOK, struct {
			Users     []localUserView `json:"users"`
			Truncated bool            `json:"truncated"`
		}{views, truncated})
	})

	mux.Handle("POST /api/v1/local-users", protect(func(w http.ResponseWriter, r *http.Request) {
		rawUsername, password, ok := localCredentialsBody(w, r)
		if !ok {
			mediaError(w, http.StatusBadRequest, "invalid_local_user_request")
			return
		}
		username, err := localusers.NormalizeUsername(rawUsername)
		if err != nil {
			mediaError(w, http.StatusBadRequest, "invalid_username")
			return
		}
		if localusers.ValidatePassword(password) != nil {
			mediaError(w, http.StatusBadRequest, "invalid_password")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		hash, err := localusers.HashPassword(ctx, password)
		if err != nil {
			unavailable(w)
			return
		}
		user, err := repo.CreateLocalUser(ctx, username, hash)
		if errors.Is(err, localusers.ErrUsernameTaken) {
			mediaError(w, http.StatusConflict, "username_taken")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusCreated, localView(user))
	}))

	for action, status := range map[string]localusers.Status{"disable": localusers.StatusDisabled, "enable": localusers.StatusActive} {
		mux.Handle("POST /api/v1/local-users/{id}/"+action, protect(func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
			if !localusers.ValidUserID(id) || err != nil || len(body) != 0 || r.URL.RawQuery != "" {
				mediaError(w, http.StatusBadRequest, "invalid_local_user_request")
				return
			}
			if repo == nil {
				unavailable(w)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			user, err := repo.SetLocalUserStatus(ctx, id, status)
			if errors.Is(err, localusers.ErrNotFound) {
				mediaError(w, http.StatusNotFound, "local_user_not_found")
				return
			}
			if err != nil {
				unavailable(w)
				return
			}
			mediaJSON(w, http.StatusOK, localView(user))
		}))
	}

	// Local sign-in needs only this Node's database, so it works with Core offline.
	mux.Handle("POST /api/v1/local-users/sessions", protect(func(w http.ResponseWriter, r *http.Request) {
		rawUsername, password, ok := localCredentialsBody(w, r)
		if !ok {
			mediaError(w, http.StatusBadRequest, "invalid_login_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		username, usernameErr := localusers.NormalizeUsername(rawUsername)
		credentials := localusers.Credentials{}
		var err error
		if usernameErr == nil && localusers.ValidatePassword(password) == nil {
			credentials, err = repo.LocalCredentials(ctx, username)
			if err != nil && !errors.Is(err, localusers.ErrNotFound) {
				unavailable(w)
				return
			}
		}
		if credentials.UserID == "" {
			if localusers.VerifyDummy(ctx, password) != nil {
				unavailable(w)
				return
			}
			localInvalidCredentials(w)
			return
		}
		matched, err := localusers.VerifyPassword(ctx, credentials.PasswordHash, password)
		if err != nil {
			unavailable(w)
			return
		}
		if !matched || credentials.Status != localusers.StatusActive {
			localInvalidCredentials(w)
			return
		}
		token, digest, err := localusers.NewSessionToken()
		if err != nil {
			unavailable(w)
			return
		}
		expiresAt, err := repo.CreateLocalSession(ctx, credentials.UserID, digest, localusers.SessionTTL)
		if errors.Is(err, localusers.ErrInactive) {
			localInvalidCredentials(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusCreated, struct {
			UserID    string `json:"user_id"`
			Kind      string `json:"kind"`
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
		}{credentials.UserID, localusers.Kind, token, expiresAt.UTC().Format(time.RFC3339)})
	}))

	mux.HandleFunc("GET /api/v1/local-users/session", func(w http.ResponseWriter, r *http.Request) {
		digest, ok := localSessionDigest(r)
		if !ok || repo == nil {
			localInvalidCredentials(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := repo.GetLocalSession(ctx, digest)
		if errors.Is(err, localusers.ErrNotFound) {
			localInvalidCredentials(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusOK, struct {
			UserID    string `json:"user_id"`
			Kind      string `json:"kind"`
			ExpiresAt string `json:"expires_at"`
		}{session.UserID, localusers.Kind, session.ExpiresAt.UTC().Format(time.RFC3339)})
	})

	mux.Handle("DELETE /api/v1/local-users/session", protect(func(w http.ResponseWriter, r *http.Request) {
		digest, ok := localSessionDigest(r)
		if !ok || repo == nil {
			localInvalidCredentials(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := repo.DeleteLocalSession(ctx, digest); err != nil {
			unavailable(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

}
