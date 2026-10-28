# Node membership invitations

A Node operator invites global users to their Node without ever seeing,
searching, or addressing the global user directory. An invitation names **no
user**: Core mints an unguessable secret, the operator shares it out of band,
and whoever holds it authenticates to Core and accepts it.

```text
Operator ──local API──► Node Agent ──signed (Node Ed25519)──► Core: create invitation
                                                     ◄── secret (returned once)
Operator ──shares secret out of band──► User
User ──Core session (bearer)──► Core: accept ──► membership(node, user, role)
```

## Creating an invitation

**Operator (local Node Agent API):**

```http
POST /api/v1/invitations
Content-Type: application/json

{"role":"MEMBER","expires_at":"2026-10-26T18:00:00Z","max_uses":1}
```

The Agent forwards exactly these three fields to Core, signed with the Node's
federation credential, and returns
`{"invitation_id","secret","role","max_uses","expires_at"}` with `201`. The
Agent does not store the secret. The endpoint has the same cross-origin
protection as the other local mutations.

| Agent status | Meaning |
| --- | --- |
| `400 invalid_invitation_request` | Malformed body or unknown fields (for example `username`). |
| `400 invalid_invitation_policy` | Core rejected the role, expiry, or max uses. |
| `409 node_not_approved` | The Node is not approved and trusted by Core. |
| `503 core_unavailable` | Core is unreachable or not enrolled. Invitations need Core. |

**Core federation endpoint (called by the Agent):**

```text
POST /api/federation/v1/nodes/{node_id}/invitations
X-ArkNet-Node-Timestamp / X-ArkNet-Node-Signature  (ARKNET-NODE-V1 proof)
```

- The Node ID is the signed path, so a Node can invite only to itself. The Node
  must be `APPROVED` and `TRUSTED`.
- The body must be exactly `role`, `expires_at`, and `max_uses`. Unknown fields,
  including user identifiers, are rejected with `400`.
- **Policy:** `role` is `MEMBER` or `ADMIN`. `expires_at` is RFC 3339, between
  1 minute and 30 days from now. `max_uses` is between 1 and 100.
- **Replay protection:** a signed Node proof is valid for ±60 s. Core stores a
  digest of the request signature (`creation_proof`, unique). Ed25519
  signatures are deterministic, so replaying a captured request returns `409`
  and creates nothing.

## Secrets

The secret is 256 random bits, base64url-encoded (43 characters). Core stores
only `SHA-256(secret)`. A fast digest is appropriate because the secret is
high-entropy and not human-chosen. The secret is returned once, travels only in
request and response bodies (never URLs), and is never logged. Share it over a
private channel: anyone holding it can accept it while it remains valid.

## Accepting an invitation

```http
POST /api/identity/v1/invitations/accept
Authorization: Bearer <Core session token>
Content-Type: application/json

{"secret":"…"}
```

The user authenticates to **Core** with a [global user](global-users.md)
session, not to the inviting Node. In one transaction, Core:

1. locks the invitation row;
2. requires it to be unexpired, have remaining uses, and belong to a Node that is
   still approved and trusted;
3. requires the user to be `ACTIVE`;
4. creates the membership `(node, user, role)`;
5. consumes one use.

The lock serializes concurrent acceptances, so uses never exceed `max_uses`.

| Status | Meaning |
| --- | --- |
| `201` | `{"node_id","member_id","role","joined_at"}`. `member_id` is the [pairwise membership ID](pairwise-membership-ids.md). |
| `401` | No valid session, or the user is disabled. |
| `404` | One identical response for unknown, malformed, expired, exhausted, or untrusted-Node invitations. |
| `409` | Already a member of that Node. **No use is consumed.** |

A single-use invitation therefore works exactly once. A multi-use invitation
admits up to `max_uses` distinct users.

## Privacy properties

- No operator-facing endpoint accepts or returns a global user ID, username, or
  any user search or listing. Invitation creation responses contain no user data.
  A Node signature carries no authority over `/api/admin/` or the identity API.
  Tests assert both.
- Core keeps the membership mapping privately. There is no Node-facing bulk
  membership route. A Node learns a [pairwise membership ID](pairwise-membership-ids.md)
  only when that member authorizes, never a global user ID or username.
- Memberships are Core-authoritative ArkNet state. Node-local users are separate
  and never involve Core.

## Not in this phase

Invitation revocation and listing, membership removal, Node-side membership
persistence, role-based permissions enforcement, and a Node UI form for invitations
(the Agent API is the operator interface for now).

## Verification

```sh
cd core && go test ./...
ARKNET_TEST_DATABASE_URL=... go test ./internal/postgres/   # real PostgreSQL
cd ../node-agent && go test ./...
```

The PostgreSQL tests cover the following, and the HTTP tests cover the same
policy end to end:

- single-use and multi-use limits;
- already-member without consuming a use;
- expiry;
- disabled users and untrusted Nodes;
- schema constraints;
- replay rejection;
- 12 concurrent users racing for 3 uses.
