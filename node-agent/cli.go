package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/application/jellyfin"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/operators"
	"arknet/node-agent/internal/postgres"
)

// Local administration commands, run inside the Agent container through
// `docker compose exec` (wrapped by `arknet-node operator …`). Local shell
// access to the Node host is the trust root for bootstrap and recovery; see
// docs/node-operator-authentication.md.
//
// Setup and recovery codes are printed to stdout only. The logger (stderr)
// receives fixed failure categories, never a code. The running Agent server
// never sees a code until an operator submits it.

type cliStore interface {
	operators.Repository
	localstate.Repository
}

type cliEnv struct {
	getenv func(string) string
	stdout io.Writer
	logger *slog.Logger
	open   func(ctx context.Context, cfg config) (cliStore, func(), error)
	// agentHealthy probes the running server's public /health; replaced in tests.
	agentHealthy func(ctx context.Context, cfg config) bool
	jellyfin     func(cfg config) application.Adapter
}

const cliUsage = `usage: arknet-agent <command>

  status [--json]                    Node status from local state (for arknet-node)
  operator setup-code [--additional] print a one-time operator setup code (1 hour)
  operator recover USERNAME          sign USERNAME out everywhere and print a recovery code
  operator list                      list operator accounts (no secrets)
  operator disable USERNAME          disable USERNAME and sign it out everywhere`

func defaultCLIEnv(getenv func(string) string, stdout, stderr io.Writer) cliEnv {
	return cliEnv{
		getenv: getenv, stdout: stdout, logger: slog.New(slog.NewJSONHandler(stderr, nil)),
		open: func(ctx context.Context, cfg config) (cliStore, func(), error) {
			store, err := postgres.Open(ctx, cfg.databaseURL)
			if err != nil {
				return nil, nil, err
			}
			return store, func() { _ = store.Close() }, nil
		},
		agentHealthy: func(ctx context.Context, cfg config) bool {
			_, port, err := net.SplitHostPort(cfg.addr)
			if err != nil {
				return false
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/health", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		},
		jellyfin: func(cfg config) application.Adapter {
			if cfg.jellyfinURL == "" {
				return nil
			}
			return jellyfin.New(cfg.jellyfinURL, nil)
		},
	}
}

// isCLI reports whether argv selects a CLI command rather than the server.
func isCLI(args []string) bool {
	return len(args) > 0 && (args[0] == "status" || args[0] == "operator" || args[0] == "help" || args[0] == "-h" || args[0] == "--help")
}

func runCLI(ctx context.Context, args []string, env cliEnv) int {
	say := func(format string, values ...any) { fmt.Fprintf(env.stdout, format+"\n", values...) }
	fail := func(category, message string) int {
		env.logger.Error("cli_failed", "reason", category)
		say("ERROR: %s", message)
		return 1
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		say("%s", cliUsage)
		return 2
	}
	cfg, err := loadConfig(env.getenv)
	if err != nil {
		return fail("configuration", "Agent configuration is invalid: "+err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, closeStore, err := env.open(ctx, cfg)
	if err != nil {
		return fail("database", "cannot reach the Node database; is the stack running?")
	}
	defer closeStore()

	switch args[0] {
	case "status":
		flags := flag.NewFlagSet("status", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		asJSON := flags.Bool("json", false, "")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			say("%s", cliUsage)
			return 2
		}
		var current *releaseSummary
		if opt := loadCurrentRelease(cfg.releaseManifest, slog.New(slog.DiscardHandler)); opt.manifest != nil {
			current = &releaseSummary{Version: opt.manifest.Version, Channel: opt.manifest.Channel}
		}
		// The heartbeat monitor lives in the server process, so live Core
		// connectivity is not known here.
		status, err := buildStatus(ctx, store, "UNKNOWN", env.jellyfin(cfg), current)
		if err != nil {
			return fail("database", "cannot read local state; has the Agent initialized the database?")
		}
		status.AgentHealthy = env.agentHealthy(ctx, cfg)
		if *asJSON {
			_ = json.NewEncoder(env.stdout).Encode(status)
			return 0
		}
		release := "none"
		if status.Release != nil {
			release = status.Release.Version
		}
		say("Node %s: %s (agent healthy %v, database healthy %v, release %s)", status.NodeID, status.State, status.AgentHealthy, status.DatabaseHealthy, release)
		return 0
	case "operator":
		return runOperatorCLI(ctx, args[1:], store, say, fail)
	}
	say("%s", cliUsage)
	return 2
}

func runOperatorCLI(ctx context.Context, args []string, store operators.Repository, say func(string, ...any), fail func(string, string) int) int {
	if len(args) == 0 {
		say("%s", cliUsage)
		return 2
	}
	username := func() (string, bool) {
		if len(args) != 2 {
			return "", false
		}
		name, err := operators.NormalizeUsername(args[1])
		return name, err == nil
	}
	switch args[0] {
	case "setup-code":
		additional := len(args) == 2 && args[1] == "--additional"
		if len(args) > 2 || len(args) == 2 && !additional {
			say("%s", cliUsage)
			return 2
		}
		code, digest, err := operators.NewSetupCode()
		if err != nil {
			return fail("random", "cannot generate a setup code")
		}
		expires, err := store.IssueSetupCode(ctx, digest, additional)
		if errors.Is(err, operators.ErrOperatorsExist) {
			return fail("operators_exist", "an operator already exists. Use `operator setup-code --additional` to add another operator, or `operator recover USERNAME`.")
		}
		if err != nil {
			return fail("database", "cannot store the setup code; has the Agent initialized the database?")
		}
		say("Operator setup code (single use, valid until %s):", expires.UTC().Format(time.RFC3339))
		say("")
		say("    %s", code)
		say("")
		say("Open the Node management UI, choose \"Set up operator\" and enter this code.")
		say("Any earlier unused setup code no longer works.")
		return 0
	case "recover":
		name, ok := username()
		if !ok {
			say("%s", cliUsage)
			return 2
		}
		code, digest, err := operators.NewSetupCode()
		if err != nil {
			return fail("random", "cannot generate a recovery code")
		}
		expires, err := store.IssueRecoveryCode(ctx, name, digest)
		if errors.Is(err, operators.ErrNotFound) {
			return fail("not_found", "no operator named "+name+". Run `operator list`.")
		}
		if err != nil {
			return fail("database", "cannot store the recovery code")
		}
		say("%s was signed out of every session.", name)
		say("Recovery code for %s (single use, valid until %s):", name, expires.UTC().Format(time.RFC3339))
		say("")
		say("    %s", code)
		say("")
		say("Open the Node management UI, choose \"Set up operator\", enter this code with username %s,", name)
		say("and set a new password and authenticator. New recovery codes are issued at the end.")
		return 0
	case "list":
		if len(args) != 1 {
			say("%s", cliUsage)
			return 2
		}
		list, err := store.ListOperators(ctx)
		if err != nil {
			return fail("database", "cannot list operators")
		}
		if len(list) == 0 {
			say("No operators. Run `operator setup-code` to create the first one.")
			return 0
		}
		var b strings.Builder
		table := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "USERNAME\tSTATUS\tCREATED\tLAST SIGN-IN")
		for _, op := range list {
			last := "never"
			if op.LastSignInAt != nil {
				last = op.LastSignInAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", op.Username, op.Status, op.CreatedAt.UTC().Format(time.RFC3339), last)
		}
		table.Flush()
		say("%s", strings.TrimRight(b.String(), "\n"))
		return 0
	case "disable":
		name, ok := username()
		if !ok {
			say("%s", cliUsage)
			return 2
		}
		err := store.DisableOperator(ctx, name)
		if errors.Is(err, operators.ErrNotFound) {
			return fail("not_found", "no operator named "+name+". Run `operator list`.")
		}
		if err != nil {
			return fail("database", "cannot disable the operator")
		}
		say("Disabled %s and signed it out of every session. `operator recover %s` re-enables it.", name, name)
		return 0
	}
	say("%s", cliUsage)
	return 2
}
