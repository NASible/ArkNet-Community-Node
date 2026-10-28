package transfer

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var (
	ErrUnavailable       = errors.New("transfer service unavailable")
	ErrUnauthorized      = errors.New("transfer unauthorized")
	ErrNotFound          = errors.New("transfer not found")
	ErrConflict          = errors.New("transfer conflict")
	ErrInvalidTransition = errors.New("transfer invalid transition")
	ErrInvalidRequest    = errors.New("transfer invalid request")
	ErrInvalidResponse   = errors.New("transfer invalid response")
)

// ValidateConfig validates an enabled client. Disabled configuration is handled by the caller.
// Errors deliberately contain no configuration values or credentials.
func ValidateConfig(baseURL, token string) error {
	if len(token) < 32 || len(token) > 256 {
		return ErrUnavailable
	}
	for _, b := range []byte(token) {
		if b < 33 || b > 126 {
			return ErrUnavailable
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(baseURL, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return ErrUnavailable
	}
	if strings.HasPrefix(u.Host, "[") {
		address, err := netip.ParseAddr(u.Hostname())
		if err != nil || !address.Is6() {
			return ErrUnavailable
		}
	} else if strings.Contains(u.Hostname(), ":") {
		return ErrUnavailable
	}
	if strings.HasSuffix(u.Host, ":") {
		return ErrUnavailable
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		address, addressErr := netip.ParseAddr(host)
		loopback := host == "localhost" || (addressErr == nil && address.IsLoopback())
		sidecar := host == "arknet-transfer"
		if !loopback && !sidecar {
			return ErrUnavailable
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return ErrUnavailable
		}
	}
	return nil
}
