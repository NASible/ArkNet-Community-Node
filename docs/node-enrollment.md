# Outbound Node Enrollment Requests

Outbound submission uses a persisted node credential and the separate Core
administration API. The Agent learns approval through signed outbound status
requests and persists `CONNECTED`. No operator identity, routing, certificates,
or network detection are implemented. Pending enrollment grants no trust.
See [the complete handshake](node-enrollment-handshake.md) for credentials,
request signing and safe legacy migration.

## Contract

Agent sends `POST /api/federation/v1/enrollments` with `Content-Type: application/json`:

```json
{
  "node_id": "<persisted node_ plus 64 lowercase hexadecimal characters>",
  "version": "0.0.1-dev",
  "capabilities": {},
  "public_key": "<standard Base64 Ed25519 public key>"
}
```

The Agent signs the exact request body using its persisted private credential;
only the public key is transmitted. These are the only application fields.
Capabilities are an empty object placeholder;
nonempty capability objects and unknown fields are rejected. The Agent does not
collect or transmit WAN IP metadata, MAC addresses, hardware serials, operator
hostnames, or filesystem data. Core does not store request headers or transport
addresses as enrollment metadata. As with any HTTP connection, the network peer
or ingress can still observe a source address; this is not transport anonymity.

Core returns the same HTTP 200 receipt for valid signed submissions, including
new, pending, decided, conflicting-key and quota-denied requests:

```json
{"node_id":"<same node ID>","enrollment_status":"PENDING"}
```

When capacity permits, Core stores a new opaque ID with `PENDING` enrollment and
`UNTRUSTED` trust. The receipt is not proof that a row exists: actual status
requires the pinned signing key. Repeated submissions do not replace existing
software version, trust, enrollment, creation time, last-seen metadata or key.
Decided records are preserved, never downgraded. Pending admission is capped at
1,000 records, expiring seven days after initial creation; retries never refresh
that deadline. No global node directory endpoint is added. See
[abuse controls](core-abuse-controls.md).

Requests are bounded to 4 KiB and strict JSON. Malformed/missing fields return
400; oversized bodies return 413; unsupported content type returns 415; invalid
proof returns 401; rate limits return 429 with Retry-After; database failure
returns a sanitized 503. Unsigned enrollment is no longer supported.

## Agent configuration and lifecycle

- `ARKNET_CORE_URL`: empty by default, disabling outbound enrollment. Configure
  an HTTPS Core base URL, e.g. `https://core.example.test`.
- `ARKNET_CORE_ALLOW_HTTP`: false by default. Explicitly enable only for a private
  development Core using HTTP. Localhost/loopback HTTP is also permitted for
  standalone development. Container `localhost` refers to that container, not Core.

URLs must not contain credentials, query strings, fragments, or a non-root path.
TLS verification remains enabled. The enrollment client does not follow redirects
or use environment-configured HTTP proxies. Requests have a bounded timeout and
honor shutdown cancellation. URL, request body, transport errors and node identity
are not logged by the enrollment path.

After local database initialization, a configured Agent submits once at startup
while `PROVISIONING` or `ENROLLMENT_PENDING`. It validates Core's response and
matching node ID before committing local pending enrollment metadata, state,
transition timestamp and audit record atomically. A repeat while pending does not
create another state transition or reset its timestamp. Approval timestamp remains
null: a successful request is not approved enrollment.

Core failure or invalid response leaves local state unchanged and logs a sanitized
failure event; the Agent still serves its local API. Initial submission failure
can be retried at restart. Once pending, the Agent checks approval outbound every
five seconds with bounded requests. Authenticated `APPROVED` / `TRUSTED` completes
enrollment and atomically persists `CONNECTED`; polling then stops. Completed
nodes do not resubmit or regress to pending at restart.
If Core commits but the response/local commit fails, a later retry safely reconciles
using the same persisted ID. No distributed transaction is claimed.

The state API remains read-only. `core_connectivity` remains `NOT_IMPLEMENTED`:
a successful enrollment POST is not continuous federation connectivity. The UI's
enrollment field remains a placeholder until a dedicated status contract is added.

## Development setup

For a real HTTPS Core, set `ARKNET_CORE_URL` in your existing `deploy/node/.env`,
then rebuild/start the Node stack:

```sh
cd deploy/node
docker compose up --build -d --wait
curl --fail http://127.0.0.1:8082/api/v1/status
```

A fresh Node should report `ENROLLMENT_PENDING` after Core acknowledges it. Keep
its PostgreSQL volume and project name so retries reuse the same identity.

For the two existing local Compose stacks on Docker Desktop, start Core normally,
then set the following in the Node's local `.env`:

```dotenv
ARKNET_CORE_URL=http://host.docker.internal:8083
ARKNET_CORE_ALLOW_HTTP=true
```

This HTTP setting is development-only, not for Internet enrollment. Docker Desktop
provides `host.docker.internal`; on Linux it requires an explicit host-gateway
mapping and a reachable host listener, or a separately configured shared private
Docker network. Do not publish PostgreSQL or broaden the local management API as
an enrollment workaround. The automated check below uses a disposable private
transport network and does not change your deployment networks.

## Verification

From repository root:

```sh
(cd core && go test ./... && go test -race ./... && go vet ./...)
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
python3 scripts/check-enrollment-stack.py
```

The integration check starts fresh isolated Core and Node databases, joins only
the two application containers on a disposable transport network, and verifies:

- Fresh Agent performs a real outbound request and enters `ENROLLMENT_PENDING`.
- Core stores exactly one untrusted pending record for its persisted node ID.
- Concurrent repeat requests do not duplicate records or overwrite metadata.
- Extra identifying request fields are rejected.
- Agent restart and Agent-only recreation preserve state, timestamp, and one
  state-history entry, despite resubmitting enrollment.
- Core and Agent repository tests run against real PostgreSQL in random schemas.

Only disposable test projects, volumes, helper images, and the temporary transport
network are removed. No local `.env` is written and your development databases
are not used.

## Security boundary

The endpoint accepts untrusted enrollment requests without proof of ownership or
administrator authentication in this phase. Knowing an ID is not identity proof.
Every accepted record stays pending/untrusted, and repeat submissions cannot
approve or revoke it. Do not treat this development endpoint as a production-ready
public registration system without admission controls, ownership proof, and the
future approval/trust workflow. No Core administrator secret belongs on a Node.

## Core enrollment administration

See [Core enrollment administration](core-enrollment-administration.md) for the separate `/api/admin/v1/` endpoints,
scoped administrator tokens, pending pagination, approval and persisted
rejection. The completed signed handshake lets the Agent learn approval and
persist `CONNECTED`; private credentials are generated locally, not issued by
Core. No Core admin UI is implemented.

## Completed enrollment handshake

The Agent now persists a unique Ed25519 credential, signs enrollment and its own
approval-status requests, and learns Core approval outbound. Approval completion
atomically records federation enrollment and `CONNECTED`; completed enrollment
and credentials survive restart without enrollment traffic. This supersedes the
request-only startup behavior described in the earlier implementation phase.
See [completed enrollment handshake](node-enrollment-handshake.md)
for protocol, legacy keyless-approval safety, storage and two-node verification.
Operator identity, mesh sessions and a Core admin UI remain outside this phase.
