package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"arknet/node-agent/internal/operators"
)

// Operator accounts, bootstrap and sessions (Prompt 45A). These routes manage
// operator identity only; protecting the management routes is Prompt 45B.
//
// Operator sessions travel only in the operatorCookie cookie. A request that
// also carries an Authorization header is rejected, so a member bearer token
// can never be mistaken for operator credentials. Setup codes, recovery codes
// and tokens are never logged; log lines carry fixed reason categories only.
const operatorCookie = "__Host-arknet_operator"

// Global bound on failed setup and sign-in attempts, protecting the argon2id
// hashing pool. Per-account delays are enforced in the database.
const maxOperatorFailuresPerMinute = 10

type operatorOpt struct {
	repository operators.Repository
	logger     *slog.Logger
	now        func() time.Time
	limiter    *failureLimiter
}

func (operatorOpt) isOption() {}

type failureLimiter struct {
	mu       sync.Mutex
	window   time.Time
	failures int
}

func (l *failureLimiter) roll(now time.Time) {
	if now.Sub(l.window) >= time.Minute {
		l.window, l.failures = now, 0
	}
}

func (l *failureLimiter) blocked(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	return l.failures >= maxOperatorFailuresPerMinute
}

func (l *failureLimiter) fail(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	l.failures++
}

// strictJSON decodes exactly one JSON object with no unknown fields.
func strictJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst) == nil && decoder.Decode(new(any)) == io.EOF
}

func setOperatorCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: token, Path: "/", Expires: expires.UTC(),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func clearOperatorCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// operatorSessionDigest reads only the operator cookie. Any Authorization
// header disqualifies the request.
func operatorSessionDigest(r *http.Request) ([32]byte, bool) {
	if len(r.Header.Values("Authorization")) != 0 {
		return [32]byte{}, false
	}
	cookie, err := r.Cookie(operatorCookie)
	if err != nil {
		return [32]byte{}, false
	}
	return operators.TokenDigest(cookie.Value)
}

type operatorSessionView struct {
	OperatorID      string    `json:"operator_id"`
	Username        string    `json:"username"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
	IdleExpiresAt   time.Time `json:"idle_expires_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func sessionView(s operators.Session) operatorSessionView {
	return operatorSessionView{s.OperatorID, s.Username, s.AuthenticatedAt.UTC(), s.IdleExpiresAt.UTC(), s.ExpiresAt.UTC()}
}

func registerOperators(mux *http.ServeMux, opt operatorOpt) {
	protect := func(h http.HandlerFunc) http.Handler { return http.NewCrossOriginProtection().Handler(h) }
	repo := opt.repository
	logger := opt.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opt.now
	if now == nil {
		now = time.Now
	}
	limiter := opt.limiter
	if limiter == nil {
		limiter = &failureLimiter{}
	}
	unavailable := func(w http.ResponseWriter) { mediaError(w, http.StatusServiceUnavailable, "operators_unavailable") }
	reject := func(w http.ResponseWriter, status int, code, reason string) {
		logger.Warn("operator_request_rejected", "reason", reason)
		mediaError(w, status, code)
	}

	// Step 1 of setup or shell recovery: redeem a code printed by
	// `arknet-agent operator setup-code` or `operator recover`.
	mux.Handle("POST /api/v1/operator/setup", protect(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SetupCode *string `json:"setup_code"`
			Username  *string `json:"username"`
			Password  *string `json:"password"`
		}
		if !strictJSON(w, r, &body) || body.SetupCode == nil || body.Username == nil || body.Password == nil {
			mediaError(w, http.StatusBadRequest, "invalid_setup_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		username, err := operators.NormalizeUsername(*body.Username)
		if err != nil || operators.ValidatePassword(*body.Password) != nil {
			mediaError(w, http.StatusBadRequest, "invalid_username_or_password")
			return
		}
		if limiter.blocked(now()) {
			reject(w, http.StatusTooManyRequests, "too_many_attempts", "rate_limited")
			return
		}
		codeDigest, ok := operators.SetupCodeDigest(*body.SetupCode)
		if !ok {
			limiter.fail(now())
			reject(w, http.StatusUnauthorized, "invalid_setup_code", "malformed_code")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		hash, err := operators.HashPassword(ctx, *body.Password)
		if err != nil {
			unavailable(w)
			return
		}
		secret, err := operators.NewTOTPSecret()
		if err != nil {
			unavailable(w)
			return
		}
		token, enrollmentDigest, err := operators.NewToken()
		if err != nil {
			unavailable(w)
			return
		}
		enrollment, err := repo.BeginEnrollment(ctx, codeDigest, username, hash, secret, enrollmentDigest)
		switch {
		case errors.Is(err, operators.ErrInvalidCode):
			limiter.fail(now())
			reject(w, http.StatusUnauthorized, "invalid_setup_code", "unknown_code")
			return
		case errors.Is(err, operators.ErrUsernameTaken):
			mediaError(w, http.StatusConflict, "username_taken")
			return
		case err != nil:
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusCreated, struct {
			EnrollmentToken string            `json:"enrollment_token"`
			Purpose         operators.Purpose `json:"purpose"`
			Username        string            `json:"username"`
			TOTPURI         string            `json:"totp_uri"`
			TOTPSecret      string            `json:"totp_secret"`
			ExpiresAt       time.Time         `json:"expires_at"`
		}{token, enrollment.Purpose, username, operators.TOTPURI(username, secret), operators.EncodeTOTPSecret(secret), enrollment.ExpiresAt.UTC()})
	}))

	// Step 2: prove the authenticator works. Returns recovery codes once and
	// starts an operator session.
	mux.Handle("POST /api/v1/operator/setup/confirm", protect(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			EnrollmentToken *string `json:"enrollment_token"`
			TOTP            *string `json:"totp"`
		}
		if !strictJSON(w, r, &body) || body.EnrollmentToken == nil || body.TOTP == nil {
			mediaError(w, http.StatusBadRequest, "invalid_confirm_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		enrollmentDigest, ok := operators.TokenDigest(*body.EnrollmentToken)
		if !ok {
			reject(w, http.StatusUnauthorized, "invalid_enrollment", "malformed_token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		enrollment, err := repo.PendingEnrollment(ctx, enrollmentDigest)
		if errors.Is(err, operators.ErrInvalidCode) {
			reject(w, http.StatusUnauthorized, "invalid_enrollment", "unknown_enrollment")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		step, ok := operators.VerifyTOTP(enrollment.TOTPSecret, *body.TOTP, now(), 0)
		if !ok {
			reject(w, http.StatusUnauthorized, "invalid_totp", "totp_mismatch")
			return
		}
		codes, digests, err := operators.NewRecoveryCodes()
		if err != nil {
			unavailable(w)
			return
		}
		token, sessionDigest, err := operators.NewToken()
		if err != nil {
			unavailable(w)
			return
		}
		session, err := repo.ConfirmEnrollment(ctx, enrollmentDigest, step, digests, sessionDigest)
		switch {
		case errors.Is(err, operators.ErrInvalidCode):
			reject(w, http.StatusUnauthorized, "invalid_enrollment", "enrollment_consumed")
			return
		case errors.Is(err, operators.ErrUsernameTaken):
			mediaError(w, http.StatusConflict, "username_taken")
			return
		case err != nil:
			unavailable(w)
			return
		}
		logger.Info("operator_enrolled", "purpose", string(enrollment.Purpose))
		setOperatorCookie(w, token, session.ExpiresAt)
		mediaJSON(w, http.StatusCreated, struct {
			Session       operatorSessionView `json:"session"`
			RecoveryCodes []string            `json:"recovery_codes"`
		}{sessionView(session), codes})
	}))

	mux.Handle("POST /api/v1/operator/sessions", protect(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username     *string `json:"username"`
			Password     *string `json:"password"`
			TOTP         *string `json:"totp"`
			RecoveryCode *string `json:"recovery_code"`
		}
		if !strictJSON(w, r, &body) || body.Username == nil || body.Password == nil || (body.TOTP == nil) == (body.RecoveryCode == nil) {
			mediaError(w, http.StatusBadRequest, "invalid_login_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		if limiter.blocked(now()) {
			reject(w, http.StatusTooManyRequests, "too_many_attempts", "rate_limited")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		invalid := func(operatorID, reason string) {
			limiter.fail(now())
			if err := repo.RecordFailedLogin(ctx, operatorID); err != nil {
				logger.Warn("operator_failure_not_recorded")
			}
			reject(w, http.StatusUnauthorized, "invalid_credentials", reason)
		}
		username, usernameErr := operators.NormalizeUsername(*body.Username)
		credentials := operators.Credentials{}
		if usernameErr == nil && operators.ValidatePassword(*body.Password) == nil {
			var err error
			credentials, err = repo.OperatorCredentials(ctx, username)
			if err != nil && !errors.Is(err, operators.ErrNotFound) {
				unavailable(w)
				return
			}
		}
		// Unknown accounts and accounts inside their failure delay take the same
		// path and time as a wrong password, without revealing which it was.
		if credentials.OperatorID == "" || credentials.RetryAfter != nil {
			if operators.VerifyDummy(ctx, *body.Password) != nil {
				unavailable(w)
				return
			}
			limiter.fail(now())
			reject(w, http.StatusUnauthorized, "invalid_credentials", "unknown_or_delayed")
			return
		}
		matched, err := operators.VerifyPassword(ctx, credentials.PasswordHash, *body.Password)
		if err != nil {
			unavailable(w)
			return
		}
		if !matched || credentials.Status != operators.StatusActive {
			invalid(credentials.OperatorID, "password_or_status")
			return
		}
		var factor operators.SecondFactor
		if body.TOTP != nil {
			step, ok := operators.VerifyTOTP(credentials.TOTPSecret, *body.TOTP, now(), credentials.TOTPLastStep)
			if !ok {
				invalid(credentials.OperatorID, "second_factor")
				return
			}
			factor.TOTPStep = step
		} else {
			digest, ok := operators.RecoveryCodeDigest(*body.RecoveryCode)
			if !ok {
				invalid(credentials.OperatorID, "second_factor")
				return
			}
			factor.RecoveryDigest = &digest
		}
		token, sessionDigest, err := operators.NewToken()
		if err != nil {
			unavailable(w)
			return
		}
		session, err := repo.CreateOperatorSession(ctx, credentials.OperatorID, factor, sessionDigest)
		if errors.Is(err, operators.ErrReplay) || errors.Is(err, operators.ErrInactive) {
			invalid(credentials.OperatorID, "second_factor_spent_or_inactive")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		setOperatorCookie(w, token, session.ExpiresAt)
		mediaJSON(w, http.StatusCreated, sessionView(session))
	}))

	mux.HandleFunc("GET /api/v1/operator/session", func(w http.ResponseWriter, r *http.Request) {
		digest, ok := operatorSessionDigest(r)
		if !ok || r.URL.RawQuery != "" {
			mediaError(w, http.StatusUnauthorized, "operator_session_required")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := repo.GetOperatorSession(ctx, digest)
		if errors.Is(err, operators.ErrNotFound) {
			clearOperatorCookie(w)
			mediaError(w, http.StatusUnauthorized, "operator_session_required")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusOK, sessionView(session))
	})

	mux.Handle("DELETE /api/v1/operator/session", protect(func(w http.ResponseWriter, r *http.Request) {
		digest, ok := operatorSessionDigest(r)
		if !ok {
			mediaError(w, http.StatusUnauthorized, "operator_session_required")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := repo.DeleteOperatorSession(ctx, digest); err != nil {
			unavailable(w)
			return
		}
		clearOperatorCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}))
}
