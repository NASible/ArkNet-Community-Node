package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"time"

	"arknet/core/internal/assertions"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

// Core-bound Node login (Prompt 46A, docs/global-login-assertions.md). The user
// signs in on this page at Core's own origin; Core then redirects the browser
// to a callback the Node registered, with a one-time signed assertion in the
// URL fragment. Passwords never reach the Node.

const (
	nodeLoginPath      = "/node-login"
	nodeLoginBodyLimit = 4096
)

// nodeLoginConfig enables Node login. published holds extra public keys that
// are listed but never used for signing: the next key before a rotation and
// retired keys until every grant they signed has expired.
type nodeLoginConfig struct {
	signer    *assertions.Signer
	published []ed25519.PublicKey
	logger    *slog.Logger
}

func registerNodeLogin(mux *http.ServeMux, cfg *nodeLoginConfig, repo assertions.Repository, identities users.Repository, handshake nodes.HandshakeRepository) {
	logger := cfg.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mux.HandleFunc("GET /api/federation/v1/assertion-keys", assertionKeysHandler(cfg))
	mux.HandleFunc("PUT /api/federation/v1/nodes/{node_id}/login-callbacks", loginCallbacksHandler(repo, handshake))
	mux.HandleFunc("GET "+nodeLoginPath, nodeLoginPageHandler(repo, logger))
	mux.Handle("POST "+nodeLoginPath, http.NewCrossOriginProtection().Handler(nodeLoginSubmitHandler(cfg.signer, repo, identities, logger)))
}

type publishedAssertionKey struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key"`
	Status    string `json:"status"`
}

func assertionKeysHandler(cfg *nodeLoginConfig) http.HandlerFunc {
	active := cfg.signer.PublicKey()
	keys := []publishedAssertionKey{{assertions.KeyIDFor(active).String(), "Ed25519", base64.RawURLEncoding.EncodeToString(active), "active"}}
	for _, public := range cfg.published {
		keys = append(keys, publishedAssertionKey{assertions.KeyIDFor(public).String(), "Ed25519", base64.RawURLEncoding.EncodeToString(public), "published"})
	}
	body, _ := json.Marshal(struct {
		Keys []publishedAssertionKey `json:"keys"`
	}{keys})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(append(body, '\n'))
	}
}

// loginCallbacksHandler lets an approved, trusted Node replace its callback set
// over its signed federation channel (decision Q2).
func loginCallbacksHandler(repo assertions.Repository, identities nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, nodeLoginBodyLimit))
		if err != nil {
			adminError(w, http.StatusBadRequest, "login callback request too large")
			return
		}
		if !authorizeOfflineNode(w, r, body, identities) {
			return
		}
		fields, ok := exactObject(body, "callback_urls")
		var urls []string
		if !ok || json.Unmarshal(fields["callback_urls"], &urls) != nil || len(urls) < 1 || len(urls) > assertions.MaxCallbackURLs {
			adminError(w, http.StatusBadRequest, "callback_urls must list 1 to 4 URLs")
			return
		}
		seen := map[string]bool{}
		for _, candidate := range urls {
			if !assertions.ValidCallbackURL(candidate) || seen[candidate] {
				adminError(w, http.StatusBadRequest, "callback URLs must be distinct HTTPS URLs with a path (HTTP only for loopback)")
				return
			}
			seen[candidate] = true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		err = repo.SetLoginCallbacks(ctx, r.PathValue("node_id"), urls)
		if errors.Is(err, assertions.ErrDenied) {
			adminError(w, http.StatusForbidden, "node is not approved")
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "login callbacks unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// nodeLoginRequest is the Node-chosen part of a login, carried from the Node's
// redirect into the form and validated again on submission.
type nodeLoginRequest struct {
	NodeID      string
	Nonce       [32]byte
	Challenge   [32]byte
	RedirectURI string
	// Form echoes the validated values back into hidden inputs.
	NonceHex, ChallengeHex string
}

var nodeLoginLinkFields = []string{"node_id", "nonce", "challenge", "redirect_uri"}

// parseNodeLoginRequest requires exactly the given fields, each once.
func parseNodeLoginRequest(values url.Values, fields ...string) (nodeLoginRequest, map[string]string, bool) {
	if len(values) != len(fields) {
		return nodeLoginRequest{}, nil, false
	}
	single := map[string]string{}
	for _, name := range fields {
		if len(values[name]) != 1 {
			return nodeLoginRequest{}, nil, false
		}
		single[name] = values[name][0]
	}
	request := nodeLoginRequest{NodeID: single["node_id"], RedirectURI: single["redirect_uri"],
		NonceHex: single["nonce"], ChallengeHex: single["challenge"]}
	var nonceOK, challengeOK bool
	request.Nonce, nonceOK = assertions.ParseHex32(single["nonce"])
	request.Challenge, challengeOK = assertions.ParseHex32(single["challenge"])
	if !nonceOK || !challengeOK || !nodes.ValidPublicID(request.NodeID) || !assertions.ValidCallbackURL(request.RedirectURI) {
		return nodeLoginRequest{}, nil, false
	}
	return request, single, true
}

// registeredCallback reports whether redirect_uri is one of this approved,
// trusted Node's registered callbacks. Unknown, unapproved and unregistered
// all look the same to the caller.
func registeredCallback(ctx context.Context, repo assertions.Repository, request nodeLoginRequest) (bool, error) {
	callbacks, err := repo.LoginCallbacks(ctx, request.NodeID)
	if errors.Is(err, assertions.ErrDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, callback := range callbacks {
		if callback == request.RedirectURI {
			return true, nil
		}
	}
	return false, nil
}

var nodeLoginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to an ArkNet Node</title></head><body>
<main>
<h1>Sign in with ArkNet</h1>
{{if .Message}}<p role="alert">{{.Message}}</p>{{end}}
{{if .Form}}
<p>You are signing in to Node <code>{{.NodeLabel}}</code>. After signing in you return to <strong>{{.Origin}}</strong>.</p>
<p>Your password is checked here, by ArkNet Core. It is never sent to the Node.</p>
<form method="post" action="` + nodeLoginPath + `">
<input type="hidden" name="node_id" value="{{.Request.NodeID}}">
<input type="hidden" name="nonce" value="{{.Request.NonceHex}}">
<input type="hidden" name="challenge" value="{{.Request.ChallengeHex}}">
<input type="hidden" name="redirect_uri" value="{{.Request.RedirectURI}}">
<p><label>Username <input name="username" autocomplete="username" required autofocus></label></p>
<p><label>Password <input name="password" type="password" autocomplete="current-password" required></label></p>
<p><button type="submit">Sign in</button></p>
</form>
{{end}}
</main></body></html>
`))

type nodeLoginView struct {
	Message   string
	Form      bool
	Request   nodeLoginRequest
	NodeLabel string
	Origin    string
}

func writeNodeLoginPage(w http.ResponseWriter, status int, view nodeLoginView) {
	formAction := "'self'"
	if view.Form {
		view.Origin = assertions.CallbackOrigin(view.Request.RedirectURI)
		view.NodeLabel = view.Request.NodeID[:17] + "…"
		formAction += " " + view.Origin
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	// same-origin (not no-referrer) so the form POST carries a real Origin
	// header for browsers that lack Sec-Fetch-Site; nothing leaves Core.
	header.Set("Referrer-Policy", "same-origin")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Content-Type-Options", "nosniff")
	// form-action also governs the redirect after submission, so it names the
	// one validated callback origin.
	header.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action "+formAction)
	w.WriteHeader(status)
	_ = nodeLoginTemplate.Execute(w, view)
}

const (
	invalidLinkMessage        = "This sign-in link is not valid. Start again from the Node."
	invalidCredentialsMessage = "Invalid username or password."
)

func nodeLoginPageHandler(repo assertions.Repository, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, _, ok := parseNodeLoginRequest(r.URL.Query(), nodeLoginLinkFields...)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if ok {
			registered, err := registeredCallback(ctx, repo, request)
			if err != nil {
				writeNodeLoginPage(w, http.StatusServiceUnavailable, nodeLoginView{Message: "Sign-in is temporarily unavailable."})
				return
			}
			ok = registered
		}
		if !ok {
			logger.Info("node_login_denied", "reason", "invalid_link")
			writeNodeLoginPage(w, http.StatusBadRequest, nodeLoginView{Message: invalidLinkMessage})
			return
		}
		writeNodeLoginPage(w, http.StatusOK, nodeLoginView{Form: true, Request: request})
	}
}

func readNodeLoginForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, nodeLoginBodyLimit))
	if err != nil {
		return nil, false
	}
	values, err := url.ParseQuery(string(body))
	return values, err == nil
}

func nodeLoginSubmitHandler(signer *assertions.Signer, repo assertions.Repository, identities users.Repository, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unavailable := func() {
			writeNodeLoginPage(w, http.StatusServiceUnavailable, nodeLoginView{Message: "Sign-in is temporarily unavailable."})
		}
		values, ok := readNodeLoginForm(w, r)
		var request nodeLoginRequest
		var fields map[string]string
		if ok {
			request, fields, ok = parseNodeLoginRequest(values, append(nodeLoginLinkFields, "username", "password")...)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if ok {
			registered, err := registeredCallback(ctx, repo, request)
			if err != nil {
				unavailable()
				return
			}
			ok = registered
		}
		if !ok {
			logger.Info("node_login_denied", "reason", "invalid_link")
			writeNodeLoginPage(w, http.StatusBadRequest, nodeLoginView{Message: invalidLinkMessage})
			return
		}
		credentials, err := authenticateGlobalUser(ctx, identities, fields["username"], fields["password"])
		if errors.Is(err, errInvalidGlobalCredentials) {
			logger.Info("node_login_denied", "reason", "invalid_credentials")
			writeNodeLoginPage(w, http.StatusUnauthorized, nodeLoginView{Message: invalidCredentialsMessage, Form: true, Request: request})
			return
		}
		if err != nil {
			unavailable()
			return
		}
		membership, err := repo.LoginMembership(ctx, request.NodeID, credentials.UserID)
		if errors.Is(err, assertions.ErrDenied) {
			logger.Info("node_login_denied", "reason", "not_member")
			writeNodeLoginPage(w, http.StatusForbidden, nodeLoginView{Message: "This account cannot sign in to this Node."})
			return
		}
		if err != nil {
			unavailable()
			return
		}
		// Claim the nonce only after authentication and membership succeed, so
		// a failed attempt cannot burn the user's sign-in link.
		err = repo.ClaimLoginNonce(ctx, request.NodeID, sha256.Sum256(request.Nonce[:]), assertions.NonceRetention)
		if errors.Is(err, assertions.ErrNonceUsed) {
			logger.Info("node_login_denied", "reason", "nonce_used")
			writeNodeLoginPage(w, http.StatusConflict, nodeLoginView{Message: "This sign-in link has already been used (for example by a second click). Start again from the Node."})
			return
		}
		if err != nil {
			unavailable()
			return
		}
		token, _, err := signer.Issue(request.NodeID, membership.MemberID, membership.Role, request.Nonce, request.Challenge)
		if err != nil {
			unavailable()
			return
		}
		logger.Info("node_login_assertion_issued", "node_id", request.NodeID, "key_id", signer.KeyID().String())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The fragment is never sent in an HTTP request, so the assertion stays
		// out of proxy, Core and Node access logs.
		w.Header().Set("Location", request.RedirectURI+"#assertion="+token)
		w.WriteHeader(http.StatusSeeOther)
	}
}

var errInvalidGlobalCredentials = errors.New("invalid global credentials")

// authenticateGlobalUser verifies a global password with the same
// enumeration-safe rules as POST /api/identity/v1/sessions: a dummy hash for
// unknown or malformed input, and status checked only after verification.
func authenticateGlobalUser(ctx context.Context, repo users.Repository, rawUsername, password string) (users.Credentials, error) {
	username, usernameErr := users.NormalizeUsername(rawUsername)
	if usernameErr != nil || users.ValidatePassword(password) != nil {
		if err := users.VerifyDummy(ctx, password); err != nil {
			return users.Credentials{}, err
		}
		return users.Credentials{}, errInvalidGlobalCredentials
	}
	credentials, err := repo.GetCredentials(ctx, username)
	if errors.Is(err, users.ErrNotFound) {
		if err := users.VerifyDummy(ctx, password); err != nil {
			return users.Credentials{}, err
		}
		return users.Credentials{}, errInvalidGlobalCredentials
	}
	if err != nil {
		return users.Credentials{}, err
	}
	matched, err := users.VerifyPassword(ctx, credentials.PasswordHash, password)
	if err != nil {
		return users.Credentials{}, err
	}
	if !matched || credentials.Status != users.StatusActive {
		return users.Credentials{}, errInvalidGlobalCredentials
	}
	return credentials, nil
}
