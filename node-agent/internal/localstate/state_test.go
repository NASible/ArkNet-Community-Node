package localstate

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAllStatePairs(t *testing.T) {
	states := []State{Provisioning, EnrollmentPending, Connected, Synchronizing, Mesh, Degraded, Offline, Reconnecting, Quarantined, Revoked, Error}
	allowed := []string{
		"ENROLLMENT_PENDING ERROR",
		"CONNECTED OFFLINE QUARANTINED REVOKED ERROR",
		"SYNCHRONIZING DEGRADED OFFLINE QUARANTINED REVOKED ERROR",
		"MESH DEGRADED OFFLINE QUARANTINED REVOKED ERROR",
		"SYNCHRONIZING DEGRADED OFFLINE QUARANTINED REVOKED ERROR",
		"RECONNECTING OFFLINE QUARANTINED REVOKED ERROR",
		"RECONNECTING QUARANTINED REVOKED ERROR",
		"ENROLLMENT_PENDING CONNECTED OFFLINE DEGRADED QUARANTINED REVOKED ERROR",
		"RECONNECTING REVOKED", "", "PROVISIONING RECONNECTING QUARANTINED REVOKED",
	}
	for i, from := range states {
		if !from.Valid() {
			t.Fatalf("invalid defined state %s", from)
		}
		for _, to := range states {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				want := strings.Contains(" "+allowed[i]+" ", " "+string(to)+" ")
				err := Validate(from, to)
				if want && err != nil {
					t.Fatalf("allowed: %v", err)
				}
				if !want && !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("forbidden: %v", err)
				}
			})
		}
	}
	for _, unknown := range []State{"", "CUSTOM_STATE", "connected"} {
		if unknown.Valid() {
			t.Fatalf("unknown accepted %q", unknown)
		}
		for _, pair := range [][2]State{{unknown, Connected}, {Connected, unknown}, {unknown, unknown}} {
			if err := Validate(pair[0], pair[1]); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("unknown: %v", err)
			}
		}
	}
}
