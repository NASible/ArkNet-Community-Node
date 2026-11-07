package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "signature", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func vectorKey(t *testing.T, name string) ed25519.PublicKey {
	t.Helper()
	key, err := ParseVerifyKey(string(vector(t, name)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return key
}

// The vectors were produced with OpenSSL 3 (`pkeyutl -sign -rawin`), so these
// tests prove the Go verifier matches the publishing toolchain.
func TestVerifySignatureAcceptsOpenSSLVector(t *testing.T) {
	if err := VerifySignature(vectorKey(t, "release-signing.pub"), vector(t, "release.json"), vector(t, "release.json.sig")); err != nil {
		t.Fatalf("valid vector rejected: %v", err)
	}
	// The signature without its optional trailing newline is equally valid.
	sig := strings.TrimSuffix(string(vector(t, "release.json.sig")), "\n")
	if err := VerifySignature(vectorKey(t, "release-signing.pub"), vector(t, "release.json"), []byte(sig)); err != nil {
		t.Fatalf("valid vector without newline rejected: %v", err)
	}
}

func TestVerifySignatureRejectsTamperingWrongKeyAndMalformed(t *testing.T) {
	key := vectorKey(t, "release-signing.pub")
	manifest := vector(t, "release.json")
	sigFile := vector(t, "release.json.sig")
	sigText := strings.TrimSuffix(string(sigFile), "\n")
	raw, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil || len(raw) != ed25519.SignatureSize {
		t.Fatalf("vector signature: %d %v", len(raw), err)
	}
	tampered := append([]byte(nil), manifest...)
	tampered[len(tampered)/2] ^= 0x01
	for name, test := range map[string]struct {
		key           ed25519.PublicKey
		manifest, sig []byte
	}{
		"tampered manifest":     {key, tampered, sigFile},
		"appended byte":         {key, append(append([]byte(nil), manifest...), ' '), sigFile},
		"wrong key":             {vectorKey(t, "other-signing.pub"), manifest, sigFile},
		"unsigned":              {key, manifest, nil},
		"empty newline":         {key, manifest, []byte("\n")},
		"bad base64":            {key, manifest, []byte("!!!!" + sigText[4:] + "\n")},
		"trailing spaces":       {key, manifest, []byte(sigText + "  ")},
		"leading space":         {key, manifest, []byte(" " + sigText)},
		"CRLF":                  {key, manifest, []byte(sigText + "\r\n")},
		"double newline":        {key, manifest, []byte(sigText + "\n\n")},
		"63 bytes":              {key, manifest, []byte(base64.StdEncoding.EncodeToString(raw[:63]) + "\n")},
		"65 bytes":              {key, manifest, []byte(base64.StdEncoding.EncodeToString(append(raw, 0)) + "\n")},
		"unpadded":              {key, manifest, []byte(strings.TrimRight(sigText, "="))},
		"url-safe alphabet":     {key, manifest, []byte(base64.URLEncoding.EncodeToString(raw))},
		"non-canonical bits":    {key, manifest, []byte(nonCanonical(sigText))},
		"signed without domain": {key, manifest, []byte(base64.StdEncoding.EncodeToString(raw))}, // replaced below
		"nil key":               {nil, manifest, sigFile},
		"short key":             {key[:31], manifest, sigFile},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "signed without domain" {
				pub, priv, _ := ed25519.GenerateKey(nil)
				test.key = pub
				test.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
			}
			if err := VerifySignature(test.key, test.manifest, test.sig); !errors.Is(err, ErrSignature) {
				t.Fatalf("accepted or wrong error: %v", err)
			}
		})
	}
}

// nonCanonical changes the unused low bits of the last base64 character
// before the padding, which lenient decoders accept as the same bytes.
func nonCanonical(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	trimmed := strings.TrimRight(s, "=")
	last := strings.IndexByte(alphabet, trimmed[len(trimmed)-1])
	return trimmed[:len(trimmed)-1] + string(alphabet[last^0x01]) + s[len(trimmed):]
}

func TestSignAndVerifyRoundTripUsesDomain(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	manifest := []byte(`{"x":1}`)
	sig := ed25519.Sign(priv, append([]byte(SignatureDomain), manifest...))
	if err := VerifySignature(pub, manifest, []byte(base64.StdEncoding.EncodeToString(sig)+"\n")); err != nil {
		t.Fatal(err)
	}
	if SignatureDomain != "arknet.node-release-signature/v1\n" {
		t.Fatalf("domain = %q", SignatureDomain)
	}
}

func TestParseVerifyKey(t *testing.T) {
	text := string(vector(t, "release-signing.pub"))
	key, err := ParseVerifyKey(text)
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("vector key: %v", err)
	}
	trimmed := strings.TrimSpace(text)
	raw, _ := base64.StdEncoding.DecodeString(trimmed)
	for name, value := range map[string]string{
		"empty":         "",
		"short":         base64.StdEncoding.EncodeToString(raw[:31]),
		"long":          base64.StdEncoding.EncodeToString(append(raw, 0)),
		"unpadded":      strings.TrimRight(trimmed, "="),
		"non-canonical": nonCanonical(trimmed),
		"url-safe":      base64.URLEncoding.EncodeToString(append([]byte{0xfb, 0xff}, raw[2:]...)),
		"not base64":    "not a key",
	} {
		if _, err := ParseVerifyKey(value); err == nil {
			t.Errorf("%s key accepted", name)
		}
	}
}

func TestRequirePinnedImages(t *testing.T) {
	m, err := Parse(vector(t, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequirePinnedImages(m); err != nil {
		t.Fatalf("pinned vector rejected: %v", err)
	}
	if m.Components[Tailscale].Bundled || strings.Contains(m.Components[Tailscale].Image, "@sha256:") {
		t.Fatal("vector should carry an unpinned, unbundled tailscale")
	}
	for name, image := range map[string]string{
		"tag only":       "jellyfin/jellyfin:12.2",
		"short digest":   "jellyfin/jellyfin:12.2@sha256:" + strings.Repeat("a", 63),
		"uppercase hex":  "jellyfin/jellyfin:12.2@sha256:" + strings.Repeat("A", 64),
		"other algo":     "jellyfin/jellyfin:12.2@sha512:" + strings.Repeat("a", 64),
		"digest in name": "jellyfin/jellyfin@sha256:" + strings.Repeat("a", 64) + ":12.2",
	} {
		changed := cloneManifest(m)
		c := changed.Components[Jellyfin]
		c.Image = image
		changed.Components[Jellyfin] = c
		if err := RequirePinnedImages(changed); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	// The locally running dev manifest uses local tags and must still parse.
	dev, err := Parse(encode(t, valid()))
	if err != nil {
		t.Fatal(err)
	}
	if RequirePinnedImages(dev) == nil {
		t.Fatal("dev manifest with local tags is not a publishable release")
	}
}

func cloneManifest(m Manifest) Manifest {
	out := m
	out.Components = map[string]Component{}
	for k, v := range m.Components {
		out.Components[k] = v
	}
	return out
}
