package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"arknet/core/internal/admins"
)

const adminUsage = `usage:
  arknet-core admin create-token --name NAME --permission PERM [--permission PERM ...] [--ttl DURATION]
  arknet-core admin revoke-token --token-id ID

Permissions: nodes.decide, users.admin, media.admin, relays.read.
DURATION is a Go duration (12h) or whole days (7d); default 30d, maximum 90d.
create-token prints the token once, as JSON. Core keeps only its SHA-256 digest.
`

type permissionFlags []string

func (p *permissionFlags) String() string     { return strings.Join(*p, ",") }
func (p *permissionFlags) Set(v string) error { *p = append(*p, v); return nil }

func parseTTL(value string) (time.Duration, bool) {
	if days, found := strings.CutSuffix(value, "d"); found {
		n, err := strconv.Atoi(days)
		if err != nil || strconv.Itoa(n) != days || n <= 0 || n > 90 {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	ttl, err := time.ParseDuration(value)
	return ttl, err == nil && admins.ValidTTL(ttl)
}

// runAdminCommand implements `arknet-core admin ...`. It is reachable only
// from a shell on the Core host; there is no HTTP route that mints tokens.
// Exit codes: 0 success, 1 operational failure, 2 usage error.
func runAdminCommand(ctx context.Context, args []string, store admins.Repository, stdout, stderr io.Writer) int {
	usage := func(message string) int {
		fmt.Fprintf(stderr, "%s\n%s", message, adminUsage)
		return 2
	}
	if len(args) == 0 {
		return usage("missing subcommand")
	}
	flags := flag.NewFlagSet("admin "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	switch args[0] {
	case "create-token":
		var permissions permissionFlags
		name := flags.String("name", "", "")
		ttlText := flags.String("ttl", "30d", "")
		flags.Var(&permissions, "permission", "")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usage("invalid arguments")
		}
		if !admins.ValidName(*name) {
			return usage("--name must be 3-64 characters of a-z, 0-9, '.', '_' or '-'")
		}
		normalized, err := admins.NormalizePermissions(permissions)
		if err != nil {
			return usage("--permission must name known permissions, at least one, without repeats")
		}
		ttl, ok := parseTTL(*ttlText)
		if !ok {
			return usage("--ttl must be between 1m and 90d")
		}
		token, digest, err := admins.NewToken()
		if err != nil {
			fmt.Fprintln(stderr, "token generation failed")
			return 1
		}
		stored, err := store.CreateToken(ctx, *name, normalized, ttl, digest)
		if err != nil {
			fmt.Fprintln(stderr, "token could not be stored")
			return 1
		}
		names := make([]string, 0, len(stored.Permissions))
		for _, p := range stored.Permissions {
			names = append(names, string(p))
		}
		err = json.NewEncoder(stdout).Encode(struct {
			Token       string   `json:"token"`
			TokenID     string   `json:"token_id"`
			AdminID     string   `json:"admin_id"`
			Admin       string   `json:"admin"`
			Permissions []string `json:"permissions"`
			ExpiresAt   string   `json:"expires_at"`
		}{token, stored.TokenID, stored.AdminID, stored.AdminName, names, stored.ExpiresAt.UTC().Format(time.RFC3339)})
		if err != nil {
			// The secret is shown only once. If delivery fails, withdraw the
			// credential even when the caller's context has already expired.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if store.RevokeToken(cleanup, stored.TokenID) != nil {
				fmt.Fprintln(stderr, "output failed; automatic revocation failed; revoke token ID", stored.TokenID)
			} else {
				fmt.Fprintln(stderr, "output failed; newly created token revoked")
			}
			return 1
		}
		return 0
	case "revoke-token":
		id := flags.String("token-id", "", "")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || !admins.ValidTokenID(*id) {
			return usage("--token-id must be an admtok_ token ID")
		}
		if err := store.RevokeToken(ctx, *id); errors.Is(err, admins.ErrNotFound) {
			fmt.Fprintln(stderr, "token not found or already revoked")
			return 1
		} else if err != nil {
			fmt.Fprintln(stderr, "token could not be revoked")
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(map[string]string{"revoked": *id}); err != nil {
			fmt.Fprintln(stderr, "output failed; token was revoked")
			return 1
		}
		return 0
	default:
		return usage("unknown subcommand " + strconv.Quote(args[0]))
	}
}
