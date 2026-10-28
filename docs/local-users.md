# Node-local users

ArkNet Nodes own local accounts independently of Core. A Node Operator can create
an account in the Node UI under **Users**. Local accounts are identified as:

```text
local_<64 lowercase hex>
```

The ID is generated from 256 random bits and is unrelated to the username. The
API and UI carry the explicit discriminator `kind: "LOCAL"`. The Users page
enumerates only these Node-local accounts; it does not expose global memberships.

## Privacy and scope

- `local_users` and `local_user_sessions` exist only in the Node Agent's
  PostgreSQL database. They have no Core foreign keys or synchronization path.
- Local user data is absent from enrollment, heartbeat, catalog, invitation,
  and membership requests. Creating, authenticating, disabling, enabling, and
  signing out a local user perform no Core request.
- A local account is valid only against the Agent and database that created it.
  A matching username on another Node creates a new random ID, password hash,
  and session namespace. It is not an account merge.
- Local authentication and the Users page keep working while Core is unavailable.
- There is no operator-facing Agent or Core route for bulk global membership
  enumeration. Per-member global authorization remains separate.

## Credential storage and sessions

Passwords are stored as argon2id PHC hashes, never plaintext. Hashing uses a
bounded concurrency pool and bounded parameters. Login failures for unknown,
malformed, wrong-password, and disabled accounts have the same HTTP 401 body;
an unknown account still performs a dummy password verification.

Sessions use 32-byte opaque bearer tokens returned once. PostgreSQL stores only
the SHA-256 digest. Sessions expire after 12 hours without sliding renewal.
Disabling an account revokes all of its sessions transactionally; enabling it
does not restore them.

## Local API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/local-users` | List this Node's local accounts. |
| `POST` | `/api/v1/local-users` | Create one from exactly `username` and `password`. |
| `POST` | `/api/v1/local-users/{local_id}/disable` | Disable and revoke sessions. |
| `POST` | `/api/v1/local-users/{local_id}/enable` | Re-enable an account. |
| `POST` | `/api/v1/local-users/sessions` | Authenticate locally and return a bearer token. |
| `GET` | `/api/v1/local-users/session` | Resolve a local bearer session. |
| `DELETE` | `/api/v1/local-users/session` | Sign out the current local session. |

Mutation routes use Go's cross-origin request protection and strict JSON
parsing. The management API still has no operator authentication in this
bootstrap release. Keep its default loopback exposure or place it behind the
Node's trusted operator access control; do not publish it directly.

## Verification

A live check on 2026-10-25 ran two compiled Node Agents against separate
PostgreSQL databases while the configured Core returned only HTTP 503:

- both Nodes created username `same-name` and established local sessions;
- Node A and Node B generated distinct random `local_…` IDs;
- each Node rejected the other Node's password for the matching username;
- both databases contained only argon2id password hashes;
- a recording Core observed four federation attempts and none contained the
  username, either local ID, either password, or either session token;
- Agent logs contained none of those values.

Handler tests repeat the offline flow, assert the removed global-members route
returns `404`, and verify local-only actions make no Core requests. Real-PostgreSQL tests cover independent stores,
session expiry, and transactional revocation. Mutation checks prove the tests
fail if local data is sent to Core, unknown-user errors become distinguishable,
or disable stops revoking sessions.

## Not in this phase

Local Jellyfin-account provisioning, using these sessions to authorize media
playback, offline authorization for cached global users, password reset, and
operator role authentication are separate work. Local users are Agent accounts;
they are not automatically Jellyfin accounts.
