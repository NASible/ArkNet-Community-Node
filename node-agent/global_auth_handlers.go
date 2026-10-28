package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

type globalAuthOpt struct {
	repository globalauth.Repository
	identity   func(context.Context) (enrollment.Identity, error)
	authorize  func(context.Context, enrollment.Identity, string, string) (globalauth.Authorization, error)
}

func (globalAuthOpt) isOption() {}

func globalCredentials(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" {
		return "", "", false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value struct {
		MemberID *string `json:"member_id"`
		Password *string `json:"password"`
	}
	if err != nil || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.MemberID == nil || value.Password == nil {
		return "", "", false
	}
	return *value.MemberID, *value.Password, true
}
func globalInvalid(w http.ResponseWriter) {
	mediaError(w, http.StatusUnauthorized, "invalid_global_credentials")
}

func registerGlobalAuth(mux *http.ServeMux, opt globalAuthOpt) {
	repo := opt.repository
	protect := func(h http.HandlerFunc) http.Handler { return http.NewCrossOriginProtection().Handler(h) }
	unavailable := func(w http.ResponseWriter) {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
	}
	mux.Handle("POST /api/v1/global-users/sessions", protect(func(w http.ResponseWriter, r *http.Request) {
		memberID, password, ok := globalCredentials(w, r)
		if !ok {
			globalInvalid(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		if repo == nil {
			unavailable(w)
			return
		}
		if !globalauth.ValidMemberID(memberID) || globalauth.ValidatePassword(password) != nil {
			if globalauth.VerifyDummy(ctx, password) != nil {
				unavailable(w)
				return
			}
			globalInvalid(w)
			return
		}
		identity := enrollment.Identity{}
		identityErr := errors.New("identity unavailable")
		if opt.identity != nil {
			identity, identityErr = opt.identity(ctx)
		}
		authorize := opt.authorize
		if authorize == nil {
			authorize = globalauth.Authorize
		}
		auth, coreErr := globalauth.Authorization{}, globalauth.ErrUnavailable
		if identityErr == nil {
			auth, coreErr = authorize(ctx, identity, memberID, password)
		}
		offline := false
		switch {
		case coreErr == nil:
			hash, err := globalauth.HashPassword(ctx, password)
			if err != nil {
				unavailable(w)
				return
			}
			auth.PasswordHash = hash
			if err = repo.CacheAuthorization(ctx, auth); err != nil {
				unavailable(w)
				return
			}
		case errors.Is(coreErr, globalauth.ErrDenied):
			if err := repo.RevokeAuthorization(ctx, memberID); err != nil {
				unavailable(w)
				return
			}
			globalInvalid(w)
			return
		case errors.Is(coreErr, globalauth.ErrUnavailable):
			offline = true
			var err error
			auth, err = repo.CachedAuthorization(ctx, memberID)
			if errors.Is(err, globalauth.ErrNotFound) {
				if globalauth.VerifyDummy(ctx, password) != nil {
					unavailable(w)
					return
				}
				globalInvalid(w)
				return
			}
			if err != nil {
				unavailable(w)
				return
			}
			matched, err := globalauth.VerifyPassword(ctx, auth.PasswordHash, password)
			if err != nil {
				unavailable(w)
				return
			}
			if !matched {
				globalInvalid(w)
				return
			}
		default:
			// Malformed or otherwise untrusted Core responses fail closed. They
			// are never treated as an outage downgrade.
			unavailable(w)
			return
		}
		token, digest, err := globalauth.NewSessionToken()
		if err != nil {
			unavailable(w)
			return
		}
		expires, err := repo.CreateGlobalSession(ctx, memberID, digest, globalauth.SessionTTL, offline)
		if errors.Is(err, globalauth.ErrInactive) {
			globalInvalid(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusCreated, struct {
			MemberID    string   `json:"member_id"`
			Kind        string   `json:"kind"`
			Permissions []string `json:"permissions"`
			Offline     bool     `json:"offline"`
			Token       string   `json:"token"`
			ExpiresAt   string   `json:"expires_at"`
		}{memberID, globalauth.Kind, auth.Permissions, offline, token, expires.UTC().Format(time.RFC3339)})
	}))
	mux.HandleFunc("GET /api/v1/global-users/session", func(w http.ResponseWriter, r *http.Request) {
		digest, ok := localSessionDigest(r)
		if !ok || repo == nil {
			globalInvalid(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := repo.GetGlobalSession(ctx, digest)
		if errors.Is(err, globalauth.ErrNotFound) {
			globalInvalid(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusOK, struct {
			MemberID    string   `json:"member_id"`
			Kind        string   `json:"kind"`
			Permissions []string `json:"permissions"`
			Offline     bool     `json:"offline"`
			ExpiresAt   string   `json:"expires_at"`
		}{session.MemberID, globalauth.Kind, session.Permissions, session.Offline, session.ExpiresAt.UTC().Format(time.RFC3339)})
	})
	mux.Handle("DELETE /api/v1/global-users/session", protect(func(w http.ResponseWriter, r *http.Request) {
		digest, ok := localSessionDigest(r)
		if !ok || repo == nil {
			globalInvalid(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := repo.DeleteGlobalSession(ctx, digest); err != nil {
			unavailable(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
