package transfer

import "testing"

func TestConfigRejectsMalformedOrRemoteHTTPAuthority(t *testing.T) {
	for _, base := range []string{"http://::1", "http://host:bad:80", "http://[not-an-ip]", "http://[127.0.0.1]", "http://[fe80::1%25en0]:8080"} {
		if err := ValidateConfig(base, testToken); err == nil {
			t.Errorf("accepted malformed authority %q", base)
		}
	}
	for _, base := range []string{"http://[::1]"} {
		if err := ValidateConfig(base, testToken); err != nil {
			t.Errorf("rejected IPv6 authority %q", base)
		}
	}
}
