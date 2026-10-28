package engine

// SeedState is the runtime-only serving state of an Agent-authorized seed.
type SeedState string

const (
	SeedChecking           SeedState = "CHECKING"
	SeedSeeding            SeedState = "SEEDING"
	SeedVerificationFailed SeedState = "VERIFICATION_FAILED"
	SeedRemoved            SeedState = "REMOVED"
)

// SeedStatus deliberately carries only the object ID and state: never URLs,
// grants, hashes or filesystem paths.
type SeedStatus struct {
	ObjectID string    `json:"object_id"`
	State    SeedState `json:"state"`
}

// ValidSeedState reports whether s is a state a live seed can be in.
func ValidSeedState(s SeedState) bool {
	return s == SeedChecking || s == SeedSeeding || s == SeedVerificationFailed
}
