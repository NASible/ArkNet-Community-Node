package main

// Core-bound global login on the member listener (Prompt 46B,
// docs/global-login-assertions.md). The Node never receives a global password:
// the browser signs in at Core, which returns a one-time Node-bound assertion
// that this Agent verifies with Core's published key set.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

const (
	// globalLoginCookie holds the browser's random login state while it signs
	// in at Core; the Node stores only its SHA-256, which is the challenge.
	globalLoginCookie = "__Host-arknet_global_login"
	// globalDeviceCookie is the per-Node device credential (offline Option A).
	globalDeviceCookie = "__Host-arknet_global_device"
	globalLoginBody    = 2048
)

// globalLoginConfig enables Core-bound login. coreURL is the Agent's
// federation URL (key set fetches); coreLoginURL is where browsers reach
// Core's login page; callbackURLs are the member UI callbacks registered
// with Core.
type globalLoginConfig struct {
	coreURL      string
	coreLoginURL string
	callbackURLs []string
}

type globalAuthOpt struct {
	repository globalauth.Repository
	identity   func(context.Context) (enrollment.Identity, error)
	login      *globalLoginConfig
	fetchKeys  func(context.Context, string) ([]globalauth.VerificationKey, error)
	now        func() time.Time
}

func (globalAuthOpt) isOption() {}

func globalInvalid(w http.ResponseWriter) {
	mediaError(w, http.StatusUnauthorized, "invalid_global_login")
}

// readGlobalLoginJSON accepts one JSON object with only the given optional
// string fields.
func readGlobalLoginJSON(w http.ResponseWriter, r *http.Request, fields ...string) (map[string]string, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" {
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, globalLoginBody))
	if err != nil {
		return nil, false
	}
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&raw) != nil || decoder.Decode(new(any)) != io.EOF || raw == nil {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, f := range fields {
		allowed[f] = true
	}
	values := map[string]string{}
	for name, value := range raw {
		var s string
		if !allowed[name] || json.Unmarshal(value, &s) != nil {
			return nil, false
		}
		values[name] = s
	}
	return values, true
}

func randomToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), sha256.Sum256(raw[:]), nil
}

// cookieDigest returns SHA-256 of a cookie holding a 32-byte random value.
func cookieDigest(r *http.Request, name string) ([32]byte, bool) {
	cookie, err := r.Cookie(name)
	if err != nil {
		return [32]byte{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cookie.Value)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256(raw), true
}

func setGlobalCookie(w http.ResponseWriter, name, value string, maxAge int, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: sameSite})
}

type globalSessionView struct {
	Mode        string   `json:"mode,omitempty"`
	MemberID    string   `json:"member_id"`
	Kind        string   `json:"kind"`
	Permissions []string `json:"permissions"`
	Offline     bool     `json:"offline"`
	Token       string   `json:"token,omitempty"`
	ExpiresAt   string   `json:"expires_at"`
}

func registerGlobalAuth(mux *routeMux, opt globalAuthOpt) {
	repo := opt.repository
	now := opt.now
	if now == nil {
		now = time.Now
	}
	fetchKeys := opt.fetchKeys
	if fetchKeys == nil {
		fetchKeys = globalauth.FetchAssertionKeys
	}
	protect := func(h http.HandlerFunc) http.Handler { return http.NewCrossOriginProtection().Handler(h) }
	unavailable := func(w http.ResponseWriter) {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
	}
	approvedIdentity := func(ctx context.Context) (enrollment.Identity, bool) {
		if opt.identity == nil {
			return enrollment.Identity{}, false
		}
		identity, err := opt.identity(ctx)
		return identity, err == nil && identity.Status == "APPROVED" && globalauth.ValidNodeID(identity.NodeID)
	}

	// The password route is retired (ARKSEC-001). It answers 410 to every
	// body and never reads a password.
	mux.HandleFunc("POST /api/v1/global-users/sessions", func(w http.ResponseWriter, r *http.Request) {
		mediaError(w, http.StatusGone, "global_password_login_removed")
	})

	mux.Handle("POST /api/v1/global-users/login", protect(func(w http.ResponseWriter, r *http.Request) {
		fields, ok := readGlobalLoginJSON(w, r, "callback_url")
		if !ok {
			mediaError(w, http.StatusBadRequest, "invalid_login_request")
			return
		}
		if repo == nil || opt.login == nil || len(opt.login.callbackURLs) == 0 {
			mediaError(w, http.StatusServiceUnavailable, "global_login_not_configured")
			return
		}
		callback := opt.login.callbackURLs[0]
		if requested, present := fields["callback_url"]; present {
			callback = ""
			for _, registered := range opt.login.callbackURLs {
				if requested == registered {
					callback = registered
				}
			}
			if callback == "" {
				mediaError(w, http.StatusBadRequest, "invalid_callback_url")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		identity, ok := approvedIdentity(ctx)
		if !ok {
			unavailable(w)
			return
		}
		keys, err := fetchKeys(ctx, opt.login.coreURL)
		switch {
		case err == nil:
			if repo.ReplaceAssertionKeys(ctx, keys) != nil {
				unavailable(w)
				return
			}
		case errors.Is(err, globalauth.ErrUnavailable):
			// Core is unreachable (transport, timeout, 5xx): only then is
			// offline login with this browser's device credential allowed.
			offlineLogin(ctx, w, r, repo, identity.NodeID)
			return
		default:
			// Malformed or untrusted Core answers fail closed.
			unavailable(w)
			return
		}
		state, challenge, err := randomToken()
		var nonce [32]byte
		if err == nil {
			_, err = rand.Read(nonce[:])
		}
		if err != nil {
			unavailable(w)
			return
		}
		if err := repo.BeginLogin(ctx, challenge, nonce, globalauth.PendingLoginTTL); err != nil {
			unavailable(w)
			return
		}
		query := url.Values{"node_id": {identity.NodeID}, "nonce": {hex.EncodeToString(nonce[:])},
			"challenge": {hex.EncodeToString(challenge[:])}, "redirect_uri": {callback}}
		setGlobalCookie(w, globalLoginCookie, state, int(globalauth.PendingLoginTTL/time.Second), http.SameSiteLaxMode)
		mediaJSON(w, http.StatusOK, struct {
			Mode         string `json:"mode"`
			AuthorizeURL string `json:"authorize_url"`
		}{"redirect", strings.TrimSuffix(opt.login.coreLoginURL, "/") + "/node-login?" + query.Encode()})
	}))

	mux.Handle("POST /api/v1/global-users/login/complete", protect(func(w http.ResponseWriter, r *http.Request) {
		fields, ok := readGlobalLoginJSON(w, r, "assertion")
		token := fields["assertion"]
		if !ok || token == "" || len(token) > 512 || repo == nil {
			globalInvalid(w)
			return
		}
		challenge, ok := cookieDigest(r, globalLoginCookie)
		if !ok {
			globalInvalid(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		nonce, err := repo.PendingLoginNonce(ctx, challenge)
		if errors.Is(err, globalauth.ErrNotFound) {
			globalInvalid(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		identity, ok := approvedIdentity(ctx)
		if !ok {
			unavailable(w)
			return
		}
		keys, err := repo.AssertionKeys(ctx)
		if err != nil {
			unavailable(w)
			return
		}
		expect := globalauth.Expectation{NodeID: identity.NodeID, Nonce: nonce, Challenge: challenge}
		claims, err := globalauth.VerifyAssertion(token, keys, expect, now())
		if errors.Is(err, globalauth.ErrUnknownAssertionKey) && opt.login != nil {
			// Core may have rotated keys since the login started: fetch once.
			if fresh, fetchErr := fetchKeys(ctx, opt.login.coreURL); fetchErr == nil && repo.ReplaceAssertionKeys(ctx, fresh) == nil {
				claims, err = globalauth.VerifyAssertion(token, fresh, expect, now())
			}
		}
		if err != nil {
			globalInvalid(w)
			return
		}
		device, deviceDigest, err := randomToken()
		if err != nil {
			unavailable(w)
			return
		}
		session, sessionDigest, err := globalauth.NewSessionToken()
		if err != nil {
			unavailable(w)
			return
		}
		login := globalauth.CompletedLogin{Challenge: challenge, AssertionID: claims.AssertionID, AssertionExpiresAt: claims.ExpiresAt,
			Grant: globalauth.Grant{MemberID: claims.MemberID, NodeID: claims.NodeID, Role: claims.Role, IssuedAt: claims.IssuedAt,
				OfflineUntil: claims.OfflineUntil, Assertion: token},
			DeviceDigest: deviceDigest, SessionDigest: sessionDigest, SessionTTL: globalauth.SessionTTL}
		if previous, ok := cookieDigest(r, globalDeviceCookie); ok {
			login.ReplacedDevice = &previous
		}
		expires, err := repo.CompleteLogin(ctx, login)
		if errors.Is(err, globalauth.ErrNotFound) || errors.Is(err, globalauth.ErrReplay) || errors.Is(err, globalauth.ErrInvalidResponse) {
			globalInvalid(w)
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		setGlobalCookie(w, globalLoginCookie, "", -1, http.SameSiteLaxMode)
		setGlobalCookie(w, globalDeviceCookie, device, int(time.Until(claims.OfflineUntil)/time.Second), http.SameSiteStrictMode)
		mediaJSON(w, http.StatusCreated, globalSessionView{MemberID: claims.MemberID, Kind: globalauth.Kind,
			Permissions: []string{globalauth.PermissionMediaAccess}, Token: session, ExpiresAt: expires.UTC().Format(time.RFC3339)})
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
		mediaJSON(w, http.StatusOK, globalSessionView{MemberID: session.MemberID, Kind: globalauth.Kind, Permissions: session.Permissions,
			Offline: session.Offline, ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339)})
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

// offlineLogin runs only when Core is unreachable. It needs this browser's
// device credential, a stored grant still inside its offline window, a sane
// clock, and a stored assertion that still verifies with a persisted Core key.
func offlineLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, repo globalauth.Repository, nodeID string) {
	device, ok := cookieDigest(r, globalDeviceCookie)
	if !ok {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
		return
	}
	rejected := func() { mediaError(w, http.StatusUnauthorized, "offline_login_rejected") }
	grant, err := repo.OfflineGrant(ctx, device)
	if errors.Is(err, globalauth.ErrNotFound) {
		rejected()
		return
	}
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
		return
	}
	keys, err := repo.AssertionKeys(ctx)
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
		return
	}
	claims, err := globalauth.VerifyStoredGrant(grant.Assertion, keys, nodeID)
	if err != nil || claims.MemberID != grant.MemberID || !claims.OfflineUntil.Equal(grant.OfflineUntil) {
		rejected()
		return
	}
	session, sessionDigest, err := globalauth.NewSessionToken()
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
		return
	}
	expires, err := repo.CreateOfflineSession(ctx, device, sessionDigest, globalauth.SessionTTL)
	if errors.Is(err, globalauth.ErrInactive) {
		rejected()
		return
	}
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "global_authentication_unavailable")
		return
	}
	mediaJSON(w, http.StatusCreated, globalSessionView{Mode: "session", MemberID: grant.MemberID, Kind: globalauth.Kind,
		Permissions: []string{globalauth.PermissionMediaAccess}, Offline: true, Token: session, ExpiresAt: expires.UTC().Format(time.RFC3339)})
}
