# Completed Node Enrollment Handshake

Enrollment is outbound and approval-gated. Operator identity is not implemented.
A node ID identifies a record; it is **not a credential** and never authorizes
credential pickup by itself.

## Lifecycle

1. Agent preserves its opaque node ID and generates one Ed25519 private seed
   locally. It persists this node-bound credential in its own PostgreSQL database
   **before** making a request. The seed is reused, not regenerated on reboot.
2. Agent submits the existing minimal enrollment metadata plus its public key,
   with a signature proving possession of the corresponding private credential.
   Core pins the first public key on a pending, untrusted record.
3. Agent records `PENDING` and enters `ENROLLMENT_PENDING` as before. It checks
   its own authenticated enrollment status outbound while waiting for approval.
4. A Core administrator approves the record, changing Core to
   `APPROVED` / `TRUSTED`. The Agent learns this through its signed status request.
5. Agent atomically persists `APPROVED`, the Core URL and enrollment timestamp,
   and `ENROLLMENT_PENDING → CONNECTED`, including the state timestamp and history.
6. After reboot, a completed Agent keeps its identity, credential, timestamps,
   and state. It does not submit another enrollment request or need Core online
   to restore this persisted enrollment. This is not a claim that the current
   transport is live; connectivity detection is a separate concern.

Polling is enrollment-only, bounded, cancelable, and stops after completion or
rejection. It is not a heartbeat, mesh discovery mechanism, or ongoing connection
monitor. Core failures do not stop the local management API or fabricate approval.
A valid rejected status never makes a node connected; no new quarantine policy
or operator-identity workflow is added.

## Credential ownership and privacy

- The unique node credential is an Ed25519 private seed stored only in the Node
  database's `node_federation_credential` table. Protect database access, backups,
  and host storage accordingly. This phase does not add at-rest key encryption.
- Core stores only a pinned public key (`nodes.enrollment_public_key`), never the
  Node private key or a reusable credential to download using a node ID.
- Core enforces public-key uniqueness across node IDs. A different public key
  cannot replace an already pinned key or reopen a decided enrollment.
- Authentication selects the public key pinned to the **target node ID**. A's
  credential does not authenticate B, even if the caller signs a request naming B.
- No private credential is returned in Agent status, administrative responses,
  public status responses, application logs, or error messages. Core administrator
  credentials are never sent to or stored by the Agent.
- Enrollment adds only the public cryptographic key required for node ownership
  proof. No WAN IP, MAC address, serial number, operator hostname, filesystem data,
  or operator identity is collected as application metadata.

Copying a complete Node database including its private key clones that node's
identity; this does not create an independently authenticated Node B. Never reuse
a Node database to provision an independently owned node.

## Protocol

The new signed request extends `POST /api/federation/v1/enrollments` with
`public_key`, the standard Base64 encoding of the 32-byte Ed25519 public key.
Other fields remain `node_id`, `version`, and `capabilities: {}`.

Approval status is read from:

```http
GET /api/federation/v1/nodes/<opaque-node-id>/enrollment
X-ArkNet-Node-Timestamp: <canonical decimal Unix seconds>
X-ArkNet-Node-Signature: <standard Base64 Ed25519 signature>
```

The exact canonical signed message is:

```text
ARKNET-NODE-V1\n<METHOD>\n<ESCAPED_PATH>\n<SHA256_BODY_HEX>\n<TIMESTAMP>\n<NODE_ID>
```

There is no trailing newline. The body digest is lowercase SHA-256 hexadecimal
of the exact raw request bytes. Status GETs have no body or query parameters.
Signatures bind method, path, body, timestamp, and node ID. Timestamp skew is
limited to 60 seconds; duplicate authentication headers are rejected.

Authenticated status returns exactly `node_id`, `enrollment_status`, and
`trust_status`, with a matching ID and one of these pairs:

- `PENDING` / `UNTRUSTED`
- `APPROVED` / `TRUSTED`
- `REJECTED` / `BLOCKED`

Unknown or keyless nodes and invalid signatures return generic HTTP 401 errors.
Core database failures return sanitized HTTP 503 errors. The Agent validates the
whole response before persisting approval. Redirects are not followed, environment
HTTP proxies are not used implicitly, and verified HTTPS remains the remote default.
HTTP development opt-in is unchanged and must not be used on untrusted networks.

These signatures cover **idempotent enrollment and read-only status only**. A
captured valid request may be replayed within the short timestamp window; it
cannot replace a key, change approval, or authenticate another node. This is not
a generic federation session or replay-safe mutation authorization system. Future
mutating federation APIs need their own replay protection and permissions.

## Persistence and upgrade boundaries

Existing migration files remain unchanged. Additive migrations store Core's
public-key binding and the Node's local private credential. State and enrollment
completion update in one transaction; failures leave the prior local state intact.
The original local state/identity initialization still must not silently rotate
identity after corruption.

Legacy pending/untrusted Core records without a key may bind the upgraded
Agent's first proven key **before** administrator approval. Legacy approvals with
no pinned key are not cryptographic approvals: they are preserved, but fail closed
for authenticated status and cannot acquire a key merely by presenting the ID.
They require deliberate Core-administrator remediation and re-approval after key
binding; this implementation does not silently reset prior decisions, issue
credentials to ID claimants, or add a public reset/recovery endpoint. Never delete
Node storage or rotate a completed credential automatically to work around this.

The old three-field submission remains a pending-only compatibility receipt. It
never yields authenticated approval or credentials; upgraded Agents use the signed
four-field flow. Core's existing approval/rejection API remains separate and uses
its explicitly development-only authorization until real administrator auth is added.

## Running and verification

Configure `ARKNET_CORE_URL` as described in [outbound enrollment](node-enrollment.md)
and enable Core's separate [administrative API](core-enrollment-administration.md)
for local development. Rebuild Core first, then Node, preserving database volumes:

```sh
(cd deploy/core && docker compose up --build -d --wait)
(cd deploy/node && docker compose up --build -d --wait)
```

List and approve the desired pending node with the Core administrative API. A
running Agent learns the result outbound; check `/api/v1/status` for `CONNECTED`.
Do not use `docker compose down --volumes` as a restart: it destroys local identity
and credential storage.

From the repository root:

```sh
(cd core && go test ./... && go test -race ./... && go vet ./...)
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
python3 scripts/check-node-handshake-stack.py
```

The live test creates an isolated Core/PostgreSQL 18 stack and two independent
Node/PostgreSQL 18 stacks, without UI containers or changes to developer `.env`
files. It verifies distinct credentials, authenticated status, A-as-B rejection,
automatic approval completion, restarts and Agent recreation with Core offline,
credential reuse, and real migration/transaction tests. Only its uniquely named
resources are removed. The Go authentication probe is integration-only and never
prints or exports private credential material. Run live stack checks sequentially
on constrained Docker VMs.
