package transfer

import (
	"net/url"
	"strings"
)

// Object IDs follow the catalog's opaque media_<sha256 hex> identity format.
func validID(id string) bool {
	return len(id) == 70 && strings.HasPrefix(id, "media_") && lowerHex(id[6:])
}
func lowerHex(s string) bool {
	for _, b := range []byte(s) {
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}
func validRequest(r Request) bool {
	return validID(r.ObjectID) && r.Size >= 0 && (r.InfoHashV2 == "" || len(r.InfoHashV2) == 64 && lowerHex(r.InfoHashV2)) && validGrantURL(r.TrackerURL) && validGrantURL(r.WebSeedURL) && validGrantURL(r.TorrentURL)
}
func validGrantURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && len(value) <= 2048 && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}
func validStatus(s Status) bool {
	if !validID(s.ObjectID) || s.TotalBytes < 0 || s.CompletedBytes < 0 || s.CompletedBytes > s.TotalBytes {
		return false
	}
	switch s.State {
	case Downloading, Paused, Cancelled:
		return s.ErrorCode == ""
	case Completed:
		return s.ErrorCode == "" && s.CompletedBytes == s.TotalBytes
	case VerificationFailed:
		return s.ErrorCode == "verification_failed"
	default:
		return false
	}
}

func validSeedRequest(r SeedRequest) bool {
	if !validID(r.ObjectID) || r.Size < 0 || len(r.InfoHashV2) != 64 || !lowerHex(r.InfoHashV2) ||
		!validGrantURL(r.TrackerURL) || !validGrantURL(r.TorrentURL) {
		return false
	}
	torrent, err := url.Parse(r.TorrentURL)
	return err == nil && strings.HasPrefix(torrent.EscapedPath(), "/api/torrent/v1/media/"+r.ObjectID+"/")
}

func validSeedStatus(s SeedStatus, removed bool) bool {
	if !validID(s.ObjectID) {
		return false
	}
	switch s.State {
	case SeedChecking, SeedSeeding, SeedVerificationFailed:
		return !removed
	case SeedRemoved:
		return removed
	default:
		return false
	}
}
