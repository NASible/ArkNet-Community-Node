package transfer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	for _, url := range []string{"http://localhost", "http://127.0.0.1:8084", "http://arknet-transfer:8084", "http://ARKNET-TRANSFER:8084", "https://example.test:443/", "http://[::1]:1234"} {
		if err := ValidateConfig(url, testToken); err != nil {
			t.Errorf("valid %s: %v", url, err)
		}
	}
	for _, url := range []string{"", "localhost", "ftp://host", "http:///", "http://user:secret@host", "http://host", "http://transfer", "http://host/base", "http://host?secret", "http://host?", "http://host#secret", "http://host#", "http://host:0", "http://host:65536", "http://host:abc", "http://host:", "http:opaque", "http://host//", "http://example.test", "http://example。test", "http://example．test", "http://example｡test", "http://2130706433", "http://017700000001", "http://0x7f000001", "http://203.0.113.10", "http://192.168.1.10", "http://transfer.example.test:8084"} {
		if ValidateConfig(url, testToken) == nil {
			t.Errorf("accepted URL %q", url)
		}
		_, err := New(url, testToken).Get(context.Background(), testID)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("bad config error %v", err)
		}
	}
	for _, token := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 257), strings.Repeat("a", 31) + " ", strings.Repeat("a", 31) + "\n", strings.Repeat("a", 31) + "é", strings.Repeat("a", 31) + "\x7f"} {
		if ValidateConfig("http://localhost", token) == nil {
			t.Error("accepted token")
		}
	}
	for _, n := range []int{32, 256} {
		if ValidateConfig("http://localhost", strings.Repeat("a", n)) != nil {
			t.Error("valid token")
		}
	}
}
