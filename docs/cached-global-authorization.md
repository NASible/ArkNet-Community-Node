# Cached global authorization

A previously approved ArkNet global member can authenticate to an authorized
Node during a temporary Core outage. Core remains authoritative whenever it is
reachable, and Nodes cache only bounded, Node-specific verification material.

## Identity and login

The Node login endpoint is:

```http
POST /api/v1/global-users/sessions
Content-Type: application/json

{"member_id":"member_…","password":"…"}
```

The member uses the pairwise membership ID returned when accepting the Node's
invitation. The Node never asks for or stores the global username or Core
`user_…` ID. A global and local account with similar names remain separate
identity domains and use different endpoints, tables, IDs, and sessions.

When Core is reachable, the Agent signs and sends the exact bounded request to:

```http
POST /api/federation/v1/nodes/{node_id}/offline-authorizations
```

Core requires an approved, trusted Node, an active global user, and membership
on that exact Node. Password verification happens inside Core. The response
contains only:

- the pairwise `member_id`;
- the allowed `node_id`;
- membership role;
- explicit permissions;
- `validated_at`;
- `expires_at`.

It contains no username, global user ID, Core password hash, or Core session.
HTTPS is required except for the existing explicit loopback/development mode.
As with any password entered into independently operated infrastructure, the
Node process handles that login attempt's plaintext password in memory; the
design does not claim a malicious Node cannot capture credentials.

## Node cache

After successful online verification, the Agent creates a fresh independently
salted argon2id verifier from the supplied password. It does **not** copy Core's
reusable password hash. Different Nodes—and later refreshes on one Node—produce
unrelated verifier strings.

`cached_global_authorizations` stores only:

- pairwise membership ID;
- this Node's ID;
- Node-specific argon2id verifier;
- `MEMBER` or `ADMIN` membership role;
- the exact offline permission `MEDIA_ACCESS`;
- validation and expiration timestamps.

The cache is limited to 1,000 active entries and each authorization expires no
later than 24 hours after validation. Expired entries are rejected and pruned.
The schema has no username, Core user ID, or global-user foreign key.

Node sessions use random 256-bit bearer tokens; PostgreSQL stores only their
SHA-256 digests. Session expiry is the earlier of 12 hours or the cached
authorization expiry. API responses use `kind: "GLOBAL"` and identify the
principal only by `member_id`.

## Online-first and fail-closed behavior

Every login tries Core first:

- successful Core authorization replaces the verifier and cached state;
- an authoritative Core denial deletes the cache and revokes its sessions;
- only transport failure, timeout, or Core 5xx permits offline verification;
- malformed success responses, redirects, unexpected status codes, wrong Node
  IDs, unknown permissions, excessive lifetimes, and protocol errors fail
  closed without cache fallback.

Unknown members, wrong passwords, malformed identifiers, expired caches, and
revoked members receive the same unauthorized response. Unknown offline
members still perform bounded dummy argon2 work.

Offline authorization grants only `MEDIA_ACCESS`. Membership administration,
invitations, role changes, password changes, account recovery, and other
security-sensitive global actions remain Core-only and unavailable offline.
The cached `ADMIN` role never creates offline administrative permission.

## Reconnect refresh

An accepted signed heartbeat triggers a separate signed refresh request for the
bounded set of cached pairwise IDs:

```http
POST /api/federation/v1/nodes/{node_id}/offline-authorizations/refresh
```

No username, password, verifier, or global ID is sent. Core returns an explicit
state for every requested pairwise ID. The Agent atomically refreshes current
roles, permissions, validation times, and expiration, or deletes revoked,
removed, and globally disabled authorizations. Applying refreshed state revokes
existing Node global sessions so stale authority cannot survive reconnection.

Refresh failure does not fabricate a heartbeat outage and does not partially
modify the cache. Existing entries continue only until their prior absolute
expiration.

## Local session API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/global-users/sessions` | Online-first global-member login with bounded offline fallback. |
| `GET` | `/api/v1/global-users/session` | Resolve an unexpired Node global session. |
| `DELETE` | `/api/v1/global-users/session` | Delete the current Node global session. |

All responses are `Cache-Control: no-store`. These session routes authenticate
a global member to this Node; they do not issue or replace a Core session.

## Verification

A live check on 2026-10-25 used compiled Core and Agent binaries with separate
PostgreSQL databases:

1. An approved global member authenticated online and populated the cache.
2. Core was stopped; the same member authenticated from the Node cache.
3. An unknown pairwise member was rejected.
4. An expired cached authorization was rejected.
5. Core restarted; the user was cached again, then Core was stopped and the
   global user disabled in Core's database.
6. On reconnect, the accepted heartbeat triggered refresh; the Agent deleted
   the authorization and revoked its existing Node session.
7. The Node schema contained no username or Core user ID, and service logs
   contained no tested password or session token.

Core and Agent suites also run against real PostgreSQL. Mutation tests prove
that authoritative-denial fallback, expiration bypass, and removal of the
reconnect hook are detected.

## Not in this phase

Passwordless/PAKE authentication, offline password changes or recovery,
unbounded member caches, offline global administration, and retroactive
revocation while Core remains unreachable are not implemented. A cache remains
usable until its absolute expiry when Core cannot communicate revocation.
