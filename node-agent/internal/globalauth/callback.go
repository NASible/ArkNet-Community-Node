package globalauth

import (
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// MaxCallbackURLs matches Core's limit for one Node's login callbacks.
const MaxCallbackURLs = 4

// ValidCallbackURL applies Core's callback rules (core/internal/assertions) so
// a misconfiguration fails at Agent start instead of at Core: a canonical
// absolute HTTPS URL with a path (plain HTTP only for loopback), an ASCII DNS
// name or IP literal, no user info, query, fragment or dot segments.
func ValidCallbackURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 512 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Opaque != "" || u.Host == "" || u.Hostname() == "" || strings.ToLower(u.Host) != u.Host {
		return false
	}
	if u.Path == "" || u.Path == "/" || path.Clean(u.Path) != u.Path || u.RawPath != "" || !validCallbackHost(u) {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

func validCallbackHost(u *url.URL) bool {
	if port := u.Port(); port != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Is4()
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
