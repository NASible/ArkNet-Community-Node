package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// adminAuthorizer isolates administrative authorization from routing and storage.
// A real identity/permission validator can replace the development implementation.
type adminAuthorizer interface{ Authorized(*http.Request) bool }

type developmentBearerAuthorizer struct {
	digest     [32]byte
	configured bool
}

func developmentAdminAuthorizer(token string) adminAuthorizer {
	return developmentBearerAuthorizer{digest: sha256.Sum256([]byte(token)), configured: validDevelopmentToken(token)}
}

func validDevelopmentToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for _, c := range token {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (a developmentBearerAuthorizer) Authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if !a.configured || len(values) != 1 {
		return false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || !validDevelopmentToken(token) {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a.digest[:], digest[:]) == 1
}
