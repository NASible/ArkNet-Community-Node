// Package release parses the versioned whole-stack ArkNet Node release
// manifest. One manifest pins every component of a Node release so that
// community operators choose only a release channel, never component versions.
package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Schema identifies version 1 of the manifest format.
const Schema = "arknet.node-release/v1"

// MaxManifestBytes bounds what the Agent will read from disk.
const MaxManifestBytes = 64 << 10

type Channel string

const (
	Stable      Channel = "stable"
	Development Channel = "development"
)

// Required component names. A manifest must pin exactly these.
const (
	Agent      = "arknet-agent"
	UI         = "arknet-ui"
	Transfer   = "arknet-transfer"
	PostgreSQL = "postgresql"
	Jellyfin   = "jellyfin"
	Tailscale  = "tailscale"
)

var RequiredComponents = []string{Agent, UI, Transfer, PostgreSQL, Jellyfin, Tailscale}

type Component struct {
	Version string `json:"version"`
	Image   string `json:"image"`
	// Bundled components run as services in the release's Compose files.
	// Unbundled components (Tailscale today) are pinned for compatibility
	// but provided by the host.
	Bundled bool `json:"bundled"`
}

type ComposeFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Compose struct {
	Project string        `json:"project"`
	Files   []ComposeFile `json:"files"`
}

type Manifest struct {
	Schema      string               `json:"schema"`
	Version     string               `json:"version"`
	Channel     Channel              `json:"channel"`
	PublishedAt time.Time            `json:"published_at"`
	Components  map[string]Component `json:"components"`
	Compose     Compose              `json:"compose"`
}

var (
	ErrInvalid  = errors.New("invalid release manifest")
	ErrMismatch = errors.New("release manifest does not describe this Agent build")
)

var (
	semver     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	compVer    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	imageRef   = regexp.MustCompile(`^([a-z0-9]+([.-][a-z0-9]+)*(:[0-9]{1,5})?/)?[a-z0-9]+([._/-][a-z0-9]+)*(:[0-9A-Za-z_][0-9A-Za-z_.-]{0,127})?(@sha256:[0-9a-f]{64})?$`)
	project    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	composeRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}\.ya?ml$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tag        = regexp.MustCompile(`^[^@]*:([0-9A-Za-z_][0-9A-Za-z_.-]*)(@|$)`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Parse strictly decodes and validates a manifest. Unknown fields, trailing
// data, missing or extra components and floating image references are rejected.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, invalid("malformed JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Manifest{}, invalid("trailing data")
	}
	return m, m.validate()
}

func (m Manifest) validate() error {
	if m.Schema != Schema {
		return invalid("unsupported schema")
	}
	if !semver.MatchString(m.Version) {
		return invalid("version must be MAJOR.MINOR.PATCH with an optional pre-release")
	}
	if m.Channel != Stable && m.Channel != Development {
		return invalid("channel must be stable or development")
	}
	if !ValidVersion(m.Version, m.Channel) {
		return invalid("stable releases must not, and development releases must, carry a pre-release suffix")
	}
	if m.PublishedAt.IsZero() {
		return invalid("published_at is required")
	}
	if len(m.Components) != len(RequiredComponents) {
		return invalid("components must be exactly %v", RequiredComponents)
	}
	for _, name := range RequiredComponents {
		c, ok := m.Components[name]
		if !ok {
			return invalid("missing component %s", name)
		}
		if !compVer.MatchString(c.Version) || !imageRef.MatchString(c.Image) {
			return invalid("component %s has an invalid version or image", name)
		}
		t := tag.FindStringSubmatch(c.Image)
		if t == nil || t[1] == "latest" || t[1] == "stable" || t[1] == "unstable" {
			return invalid("component %s image must be pinned to a version tag", name)
		}
	}
	for _, name := range []string{Agent, UI, Transfer} {
		c := m.Components[name]
		// ArkNet-owned components ship in lockstep with the release.
		if c.Version != m.Version || !c.Bundled {
			return invalid("component %s must be bundled at the release version", name)
		}
	}
	if !m.Components[PostgreSQL].Bundled || !m.Components[Jellyfin].Bundled {
		return invalid("postgresql and jellyfin must be bundled")
	}
	if !project.MatchString(m.Compose.Project) || len(m.Compose.Files) == 0 || len(m.Compose.Files) > 8 {
		return invalid("compose must name a project and 1-8 files")
	}
	seen := map[string]bool{}
	for _, f := range m.Compose.Files {
		if !composeRef.MatchString(f.Path) || !sha256Hex.MatchString(f.SHA256) || seen[f.Path] {
			return invalid("compose files must be unique file names with SHA-256 digests")
		}
		seen[f.Path] = true
	}
	return nil
}

// ValidVersion reports whether version is a release version permitted on
// channel: stable releases have no pre-release suffix, development ones do.
func ValidVersion(version string, channel Channel) bool {
	match := semver.FindStringSubmatch(version)
	if match == nil {
		return false
	}
	switch channel {
	case Stable:
		return match[4] == ""
	case Development:
		return match[4] != ""
	}
	return false
}

// Compare orders two release versions by SemVer 2.0 precedence, returning
// -1, 0 or 1. Both must be valid release versions.
func Compare(a, b string) (int, error) {
	ma, mb := semver.FindStringSubmatch(a), semver.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, invalid("cannot compare invalid versions")
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumeric(ma[i], mb[i]); c != 0 {
			return c, nil
		}
	}
	pa, pb := strings.TrimPrefix(ma[4], "-"), strings.TrimPrefix(mb[4], "-")
	switch {
	case pa == pb:
		return 0, nil
	case pa == "":
		return 1, nil // a release outranks its pre-releases
	case pb == "":
		return -1, nil
	}
	fa, fb := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(fa) && i < len(fb); i++ {
		na, nb := numeric(fa[i]), numeric(fb[i])
		var c int
		switch {
		case na && nb:
			c = compareNumeric(fa[i], fb[i])
		case na:
			c = -1
		case nb:
			c = 1
		default:
			c = strings.Compare(fa[i], fb[i])
		}
		if c != 0 {
			return c, nil
		}
	}
	return compareInt(len(fa), len(fb)), nil
}

func numeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// compareNumeric compares unsigned decimal strings of any length.
func compareNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if c := compareInt(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Load reads and parses a manifest file of bounded size.
func Load(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > MaxManifestBytes {
		return Manifest{}, invalid("manifest too large")
	}
	return Parse(data)
}

// LoadCurrent loads the manifest for the running release and confirms it
// describes this Agent build, so the UI never reports a release the Node is
// not actually running.
func LoadCurrent(path, agentVersion string) (Manifest, error) {
	m, err := Load(path)
	if err != nil {
		return Manifest{}, err
	}
	if m.Components[Agent].Version != agentVersion {
		return Manifest{}, ErrMismatch
	}
	return m, nil
}
