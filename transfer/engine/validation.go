package engine

import "strings"

func LowerHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidObjectID(s string) bool {
	return strings.HasPrefix(s, "media_") && LowerHex(strings.TrimPrefix(s, "media_"))
}
