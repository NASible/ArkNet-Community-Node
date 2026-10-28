package release

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func valid() map[string]any {
	digest := strings.Repeat("a", 64)
	comp := func(version, image string, bundled bool) map[string]any {
		return map[string]any{"version": version, "image": image, "bundled": bundled}
	}
	return map[string]any{
		"schema": Schema, "version": "0.1.0-dev.1", "channel": "development", "published_at": "2026-10-25T00:00:00Z",
		"components": map[string]any{
			Agent: comp("0.1.0-dev.1", "arknet-agent:0.1.0-dev.1", true),
			UI:    comp("0.1.0-dev.1", "ghcr.io/arknet/arknet-ui:0.1.0-dev.1", true),
			// Registry hosts may carry a port.
			Transfer:   comp("0.1.0-dev.1", "registry.example:5000/arknet/arknet-transfer:0.1.0-dev.1", true),
			PostgreSQL: comp("18", "postgres:18-alpine", true),
			Jellyfin:   comp("12.2", "jellyfin/jellyfin:12.2@sha256:"+digest, true),
			Tailscale:  comp("1.104.2", "tailscale/tailscale:v1.104.2", false),
		},
		"compose": map[string]any{"project": "arknet-node", "files": []any{map[string]any{"path": "compose.yaml", "sha256": digest}}},
	}
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func components(m map[string]any) map[string]any { return m["components"].(map[string]any) }
func component(m map[string]any, name string) map[string]any {
	return components(m)[name].(map[string]any)
}

func TestParseAcceptsStableAndDevelopmentReleases(t *testing.T) {
	m, err := Parse(encode(t, valid()))
	if err != nil || m.Version != "0.1.0-dev.1" || m.Channel != Development || m.Components[Tailscale].Bundled || m.Compose.Files[0].Path != "compose.yaml" {
		t.Fatalf("parse = %+v, %v", m, err)
	}
	stable := valid()
	stable["version"], stable["channel"] = "0.1.0", "stable"
	for _, name := range []string{Agent, UI, Transfer} {
		c := component(stable, name)
		c["version"] = "0.1.0"
		c["image"] = strings.Replace(c["image"].(string), "0.1.0-dev.1", "0.1.0", 1)
	}
	if m, err := Parse(encode(t, stable)); err != nil || m.Channel != Stable || m.Version != "0.1.0" {
		t.Fatalf("stable = %+v, %v", m, err)
	}
}

func TestParseRejectsInvalidManifests(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"schema":                  func(m map[string]any) { m["schema"] = "arknet.node-release/v2" },
		"unknown field":           func(m map[string]any) { m["extra"] = true },
		"non-semver":              func(m map[string]any) { m["version"] = "v0.1" },
		"build metadata":          func(m map[string]any) { m["version"] = "0.1.0+abc" },
		"leading zero":            func(m map[string]any) { m["version"] = "0.01.0-dev" },
		"stable with pre-release": func(m map[string]any) { m["channel"] = "stable" },
		"development without pre-release": func(m map[string]any) {
			m["version"] = "0.1.0"
			for _, n := range []string{Agent, UI, Transfer} {
				component(m, n)["version"] = "0.1.0"
			}
		},
		"unknown channel":    func(m map[string]any) { m["channel"] = "beta" },
		"missing published":  func(m map[string]any) { delete(m, "published_at") },
		"missing component":  func(m map[string]any) { delete(components(m), Tailscale) },
		"extra component":    func(m map[string]any) { components(m)["redis"] = component(m, PostgreSQL) },
		"latest tag":         func(m map[string]any) { component(m, Jellyfin)["image"] = "jellyfin/jellyfin:latest" },
		"untagged image":     func(m map[string]any) { component(m, PostgreSQL)["image"] = "postgres" },
		"untagged with port": func(m map[string]any) { component(m, Transfer)["image"] = "registry.example:5000/arknet-transfer" },
		"digest only": func(m map[string]any) {
			component(m, Jellyfin)["image"] = "jellyfin/jellyfin@sha256:" + strings.Repeat("a", 64)
		},
		"stable tag":         func(m map[string]any) { component(m, Tailscale)["image"] = "tailscale/tailscale:stable" },
		"bad image":          func(m map[string]any) { component(m, UI)["image"] = "Arknet UI" },
		"agent not lockstep": func(m map[string]any) { component(m, Agent)["version"] = "0.0.9" },
		"ui unbundled":       func(m map[string]any) { component(m, UI)["bundled"] = false },
		"jellyfin unbundled": func(m map[string]any) { component(m, Jellyfin)["bundled"] = false },
		"empty version":      func(m map[string]any) { component(m, PostgreSQL)["version"] = "" },
		"no compose files":   func(m map[string]any) { m["compose"].(map[string]any)["files"] = []any{} },
		"compose path escape": func(m map[string]any) {
			m["compose"].(map[string]any)["files"].([]any)[0].(map[string]any)["path"] = "../compose.yaml"
		},
		"compose bad digest": func(m map[string]any) {
			m["compose"].(map[string]any)["files"].([]any)[0].(map[string]any)["sha256"] = "abc"
		},
		"duplicate compose": func(m map[string]any) {
			c := m["compose"].(map[string]any)
			c["files"] = append(c["files"].([]any), c["files"].([]any)[0])
		},
		"bad project": func(m map[string]any) { m["compose"].(map[string]any)["project"] = "ArkNet Node" },
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(m)
			if _, err := Parse(encode(t, m)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	for _, raw := range []string{"", "[]", "{}", string(encode(t, valid())) + "{}"} {
		if _, err := Parse([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q accepted", raw)
		}
	}
}

func TestLoadCurrentRequiresThisAgentBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(path, encode(t, valid()), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadCurrent(path, "0.1.0-dev.1"); err != nil || m.Version != "0.1.0-dev.1" {
		t.Fatalf("load = %+v %v", m, err)
	}
	if _, err := LoadCurrent(path, "0.1.0-dev.2"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := LoadCurrent(filepath.Join(t.TempDir(), "missing.json"), "0.1.0-dev.1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", MaxManifestBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized = %v", err)
	}
}
