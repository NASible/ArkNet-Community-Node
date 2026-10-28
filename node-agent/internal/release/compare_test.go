package release

import "testing"

func TestCompareFollowsSemVerPrecedence(t *testing.T) {
	ordered := []string{
		"0.0.1-dev", "0.0.1-dev.1", "0.0.1-dev.2", "0.0.1-dev.10", "0.0.1-dev.x", "0.0.1-rc.1", "0.0.1",
		"0.0.2-dev", "0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2",
		"0.1.0-beta.11", "0.1.0-rc.1", "0.1.0", "0.10.0", "1.0.0", "2.0.0", "10.0.0", "99999999999999999999.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			got, err := Compare(ordered[i], ordered[j])
			want := compareInt(i, j)
			if err != nil || got != want {
				t.Fatalf("Compare(%s, %s) = %d %v, want %d", ordered[i], ordered[j], got, err, want)
			}
		}
	}
	for _, bad := range [][2]string{{"v1.0.0", "1.0.0"}, {"1.0", "1.0.0"}, {"1.0.0", "1.0.0+b"}} {
		if _, err := Compare(bad[0], bad[1]); err == nil {
			t.Fatalf("compared invalid %v", bad)
		}
	}
}

func TestValidVersionPerChannel(t *testing.T) {
	for version, want := range map[string][2]bool{ // {stable, development}
		"0.1.0": {true, false}, "0.1.0-dev.1": {false, true}, "v0.1.0": {false, false}, "0.1": {false, false},
	} {
		if ValidVersion(version, Stable) != want[0] || ValidVersion(version, Development) != want[1] || ValidVersion(version, "beta") {
			t.Fatalf("%s: unexpected channel validity", version)
		}
	}
}
