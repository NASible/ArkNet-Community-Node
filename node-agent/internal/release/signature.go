package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SignatureDomain prefixes the manifest bytes in the signed message so a
// release signature can never be replayed as any other ArkNet signature.
// The signed message is SignatureDomain followed by the exact manifest bytes.
const SignatureDomain = "arknet.node-release-signature/v1\n"

// MaxSignatureBytes bounds the detached signature file (base64 of 64 bytes
// plus an optional newline is 89 bytes).
const MaxSignatureBytes = 256

// ErrSignature marks a missing, malformed or non-verifying release signature.
var ErrSignature = errors.New("release signature rejected")

var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// ParseVerifyKey decodes a release verification key: canonical padded
// standard base64 of a raw 32-byte Ed25519 public key. Surrounding
// whitespace is ignored.
func ParseVerifyKey(s string) (ed25519.PublicKey, error) {
	value := strings.TrimSpace(s)
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != value {
		return nil, errors.New("release verify key must be canonical padded base64 of a 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

// VerifySignature checks a detached signature file over manifest. The file
// must be canonical padded standard base64 of the 64-byte signature,
// optionally followed by exactly one "\n"; nothing else is accepted.
func VerifySignature(key ed25519.PublicKey, manifest, sigFile []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: no valid verify key", ErrSignature)
	}
	if len(sigFile) > MaxSignatureBytes {
		return fmt.Errorf("%w: signature file too large", ErrSignature)
	}
	text := string(bytes.TrimSuffix(sigFile, []byte("\n")))
	sig, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != text {
		return fmt.Errorf("%w: malformed signature file", ErrSignature)
	}
	message := make([]byte, 0, len(SignatureDomain)+len(manifest))
	message = append(append(message, SignatureDomain...), manifest...)
	if !ed25519.Verify(key, message, sig) {
		return fmt.Errorf("%w: signature does not verify", ErrSignature)
	}
	return nil
}

// RequirePinnedImages enforces the published-release rule: every bundled
// component image ends in "@sha256:" and 64 lowercase hex digits. Unbundled
// components (provided by the host) are exempt. It is applied to manifests
// found by update discovery, not to the locally running manifest.
func RequirePinnedImages(m Manifest) error {
	for _, name := range RequiredComponents {
		c, ok := m.Components[name]
		if !ok {
			return invalid("missing component %s", name)
		}
		if c.Bundled && !pinnedImage.MatchString(c.Image) {
			return invalid("published component %s image must be pinned by sha256 digest", name)
		}
	}
	return nil
}
