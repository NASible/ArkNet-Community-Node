package routing

import "testing"

func TestDecidePrefersOnlyHealthyHomeNodeWithVerifiedMedia(t *testing.T) {
	const home = "node_" + "a"
	for _, test := range []struct {
		name   string
		home   string
		status HomeNodeStatus
		want   Decision
	}{
		{"available on healthy home", home, HomeNodeStatus{Healthy: true, VerifiedAvailable: true}, Decision{TargetHomeNode, home}},
		{"missing on healthy home", home, HomeNodeStatus{Healthy: true}, Decision{Target: TargetCore}},
		{"offline home with media", home, HomeNodeStatus{VerifiedAvailable: true}, Decision{Target: TargetCore}},
		{"offline home without media", home, HomeNodeStatus{}, Decision{Target: TargetCore}},
		{"no home node", "", HomeNodeStatus{Healthy: true, VerifiedAvailable: true}, Decision{Target: TargetCore}},
	} {
		if got := Decide(test.home, test.status); got != test.want {
			t.Fatalf("%s: got %+v want %+v", test.name, got, test.want)
		}
	}
}
