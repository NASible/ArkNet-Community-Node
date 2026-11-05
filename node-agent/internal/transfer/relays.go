package transfer

import (
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relayID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
var relayHostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var virtualRelayNetwork = netip.MustParsePrefix("100.64.0.0/10")

func validRelays(relays []Relay, expires int64, now time.Time) bool {
	if len(relays) == 0 {
		return expires == 0
	}
	if len(relays) > 16 || expires <= now.Unix() {
		return false
	}
	ids, pins, addresses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range relays {
		if !relayID.MatchString(r.ID) || len(r.SPKISHA256) != 64 || !lowerHex(r.SPKISHA256) || !validRelayAddress(r.Address) || ids[r.ID] || pins[r.SPKISHA256] || addresses[r.Address] {
			return false
		}
		ids[r.ID], pins[r.SPKISHA256], addresses[r.Address] = true, true, true
	}
	return true
}

// Relay addresses name real TLS listeners, not virtual swarm endpoints or URLs.
func validRelayAddress(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == "" && ip.String() == host && !ip.Is4In6() && !virtualRelayNetwork.Contains(ip.Unmap()) && net.JoinHostPort(host, portText) == address
	}
	if len(host) > 253 || net.JoinHostPort(host, portText) != address {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !relayHostLabel.MatchString(label) {
			return false
		}
	}
	return true
}
