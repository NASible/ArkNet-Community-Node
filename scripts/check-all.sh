#!/usr/bin/env bash
# One-command ArkNet gate.
#
#   scripts/check-all.sh            component gate (no long-running stacks; uses a
#                                   throwaway PostgreSQL container for store tests)
#   scripts/check-all.sh --e2e      + Docker acceptance: MVP E2E, Node upgrade, product swarm
#   scripts/check-all.sh --vm       + fresh-VM install check (needs Lima)
#   --logs DIR                      keep step logs and diagnostics in DIR (default: a new temp dir)
#
# Every step runs even if an earlier one fails. Step logs are kept; failing
# acceptance checks also write sanitized diagnostics bundles under DIR. The last
# line is always "EXIT <n>", where n is the number of failed steps.
set -u

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT" || exit 1

RUN_E2E=0
RUN_VM=0
LOGS=""
while [ $# -gt 0 ]; do
    case "$1" in
        --e2e) RUN_E2E=1 ;;
        --vm) RUN_VM=1 ;;
        --logs) shift; LOGS=${1:-} ;;
        -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
        *) printf 'unknown option: %s\n' "$1" >&2; exit 2 ;;
    esac
    shift
done
if [ -z "$LOGS" ]; then
    LOGS=$(mktemp -d "${TMPDIR:-/tmp}/arknet-check-all.XXXXXX")
fi
mkdir -p "$LOGS"
GO_MODULES="core node-agent tracker transfer relay"
WORKER_IMAGE="arknet-check-all-transfer:local"
FAILED=""
PASSED=0

step() {  # step <name> <command...>
    local name=$1; shift
    local log="$LOGS/$name.log"
    local started=$SECONDS
    printf 'STEP %-24s ' "$name"
    if "$@" >"$log" 2>&1; then
        PASSED=$((PASSED + 1))
        printf 'PASS (%ss)\n' $((SECONDS - started))
    else
        FAILED="$FAILED $name"
        printf 'FAIL (%ss) — see %s\n' $((SECONDS - started)) "$log"
        tail -n 15 "$log" | sed 's/^/    | /'
    fi
}

gofmt_clean() {
    local unformatted
    unformatted=$(gofmt -l $GO_MODULES)
    [ -z "$unformatted" ] || { printf 'gofmt needed:\n%s\n' "$unformatted"; return 1; }
}

go_module() {  # go_module <dir>
    (cd "$1" && go test ./... && go vet ./...)
}

node_ui() {
    (cd node-ui && { [ -d node_modules ] || npm ci; } && npm test -- --run && npm run build)
}

harness_tests() {
    local modules
    modules=$(cd scripts && ls *_test.py | sed 's/\.py$//; s/^/scripts./')
    # shellcheck disable=SC2086
    python3 -m unittest -v $modules
}

deploy_tests() {
    local modules
    modules=$(cd deploy/node && ls *_test.py | sed 's/\.py$//; s/^/deploy.node./')
    # shellcheck disable=SC2086
    python3 -m unittest -v $modules
}

# Go store tests skip without ARKNET_TEST_DATABASE_URL. Run them against a
# throwaway PostgreSQL matching the shipped image, from a Go container sharing
# its network namespace: the tests then share the database's clock, as Core and
# the Agent do in production (Docker Desktop's VM clock can drift from the host).
postgres_tests() {
    local name="arknet-check-all-postgres-$$" password status=0 modcache
    password=$(openssl rand -hex 16)
    modcache=$(go env GOMODCACHE)
    docker run -d --rm --name "$name" -e POSTGRES_PASSWORD="$password" postgres:18-alpine >/dev/null || return 1
    for _ in $(seq 1 60); do
        docker exec "$name" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1 && break
        sleep 1
    done
    sleep 1
    docker run --rm --network "container:$name" -e GOFLAGS=-mod=mod -e GOTOOLCHAIN=local \
        -e ARKNET_TEST_DATABASE_URL="postgres://postgres:${password}@127.0.0.1:5432/postgres?sslmode=disable" \
        -v "$ROOT:/src:ro" -v "$modcache:/go/pkg/mod" -w /src golang:1.26-alpine sh -c \
        'export GOCACHE=/tmp/gocache; cd /src/core && go test -count=1 ./internal/postgres/... . && cd /src/node-agent && go test -count=1 ./internal/postgres/... .' \
        || status=$?
    docker rm -f "$name" >/dev/null 2>&1
    return $status
}

worker_tests() {
    docker build -q -t "$WORKER_IMAGE" transfer >/dev/null || return 1
    local status=0
    docker run --rm --network none -u 10001:10001 -v "$ROOT/transfer:/w:ro" -w /w --entrypoint python3 \
        "$WORKER_IMAGE" -B -m unittest -v worker_test worker_seed_test worker_relay_test \
        worker_connector_test relay_connector_test egress_policy_test || status=$?
    # Real Go relay + tracker with two libtorrent sessions: relayed seed -> download.
    local bin arch cid
    bin=$(mktemp -d "${TMPDIR:-/tmp}/arknet-relay-bin.XXXXXX")
    arch=$(docker version --format '{{.Server.Arch}}')
    if (cd relay && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$bin/relay" .) &&
        (cd tracker && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$bin/tracker" .); then
        # mktemp creates 0700 and docker cp keeps the mode; UID 10001 must traverse it.
        chmod 755 "$bin" "$bin/relay" "$bin/tracker"
        # docker cp, not a bind mount: the temp dir may be outside the VM's shared paths.
        if cid=$(docker create --network none -u 10001:10001 -v "$ROOT/transfer:/w:ro" \
                --tmpfs /work:uid=10001,gid=10001 -w /w -e ARKNET_TEST_TMPDIR=/work \
                -e ARKNET_TEST_RELAY_BINARY=/probe/relay -e ARKNET_TEST_TRACKER_BINARY=/probe/tracker \
                --entrypoint python3 "$WORKER_IMAGE" -B -m unittest -v relay_integration_test); then
            { docker cp "$bin" "$cid:/probe" && docker start -a "$cid"; } || status=$?
            docker rm -f "$cid" >/dev/null 2>&1
        else
            status=1
        fi
    else
        status=1
    fi
    rm -rf "$bin"
    docker image rm -f "$WORKER_IMAGE" >/dev/null 2>&1
    return $status
}

e2e() {
    python3 -u scripts/check-mvp-e2e.py --results "$LOGS/e2e.json" --diagnostics-dir "$LOGS/e2e-diagnostics"
}

swarm() {
    python3 -u scripts/check-multi-node-swarm-stack.py --results "$LOGS/swarm.json" \
        --diagnostics-dir "$LOGS/swarm-diagnostics"
}

printf 'ArkNet gate: %s\nLogs: %s\n' "$(git rev-parse --short HEAD 2>/dev/null || echo 'no git')" "$LOGS"
step scaffold sh scripts/check-scaffold.sh
step hygiene python3 scripts/check-security-hygiene.py
step gofmt gofmt_clean
for module in $GO_MODULES; do
    step "go-$module" go_module "$module"
done
step node-ui node_ui
step harness-tests harness_tests
step deploy-tests deploy_tests
step postgres-tests postgres_tests
step worker-tests worker_tests
if [ "$RUN_E2E" = 1 ]; then
    step mvp-e2e e2e
    step node-upgrade python3 -u scripts/check-node-upgrade-stack.py
    step product-swarm swarm
fi
if [ "$RUN_VM" = 1 ]; then
    step node-install-vm python3 -u scripts/check-node-install-vm.py
fi

count=$(printf '%s\n' $FAILED | grep -c . || true)
printf '\nSUMMARY: %d passed, %d failed%s\n' "$PASSED" "$count" "${FAILED:+ (${FAILED# })}"
printf 'Logs: %s\n' "$LOGS"
printf 'EXIT %d\n' "$count"
exit "$count"
