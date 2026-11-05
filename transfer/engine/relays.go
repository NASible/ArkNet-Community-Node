package engine

import (
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Relay is a runtime-only pinned endpoint from the Agent's verified relay list.
// It must never be included in a status response or diagnostic.
type Relay struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	SPKISHA256 string `json:"spki_sha256"`
}

var relayID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
var relayLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var virtualRelayNetwork = netip.MustParsePrefix("100.64.0.0/10")

// ValidRelays validates the entire replacement list. Empty lists require zero
// expiry and mean web-seed-only operation, never permission for direct peers.
func ValidRelays(relays []Relay, expiresAt int64, now time.Time) bool {
	if len(relays) == 0 {
		return expiresAt == 0
	}
	if len(relays) > 16 || expiresAt <= now.Unix() {
		return false
	}
	ids, addresses, pins := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range relays {
		if !relayID.MatchString(r.ID) || !validRelayAddress(r.Address) || !LowerHex(r.SPKISHA256) || ids[r.ID] || addresses[r.Address] || pins[r.SPKISHA256] {
			return false
		}
		ids[r.ID], addresses[r.Address], pins[r.SPKISHA256] = true, true, true
	}
	return true
}

func validRelayAddress(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || len(host) == 0 || len(host) > 253 || net.JoinHostPort(host, portText) != address {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == "" && ip.String() == host && !ip.Is4In6() && !virtualRelayNetwork.Contains(ip.Unmap())
	}
	for _, label := range strings.Split(host, ".") {
		if !relayLabel.MatchString(label) {
			return false
		}
	}
	return true
}
