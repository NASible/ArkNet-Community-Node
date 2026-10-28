# ArkNet global users

Core owns ArkNet global user accounts. There is no external identity provider,
federation login, or OIDC integration. Global users are distinct from Node
local-only users, which never synchronize to Core.

## MVP account mechanism

Administrators create accounts through the protected Core administrative API.
There is **no open self-registration** endpoint in this phase. User
administration sits behind the same replaceable `adminAuthorizer` as Node
enrollment administration (today: the explicit development bearer token; see
[Core enrollment administration](core-enrollment-administration.md)). When the
administrative authorizer is not configured, the entire namespace returns 404.

There is deliberately no user listing endpoint: the global user directory is not
browsable, by Node operators or through this API.

## Account model

| Field | Rules |
| --- | --- |
| `user_id` | `user_` + 64 lowercase hex characters from 256 random bits. Stable, opaque, and derived from no user input. |
| `username` | Login handle only. Normalized to lowercase; 3–32 characters of `a-z 0-9 . _ -`, starting with a letter or digit; unique. Never sent to Nodes. |
| password | 12–256 bytes of valid UTF-8; no composition rules. Stored only as an argon2id hash. |
| `status` | `ACTIVE` or `DISABLED`. |

Nodes will see pairwise pseudonymous IDs derived per Node in a later phase, not
`user_id` or `username`.

## Credential storage

- Passwords are hashed with **argon2id** (OWASP minimum parameters: m=19 MiB,
  t=2, p=1, 16-byte random salt, 32-byte key) and stored as a PHC string. The
  schema rejects any `password_hash` that is not an argon2id string.
- Verification uses constant-time comparison. It also bounds the parameters
  read from storage, so a corrupted row can't force unbounded memory or CPU use.
- At most four hashes run concurrently (about 76 MiB in total), so floods of
  login attempts cannot exhaust Core memory. A request that cannot get a hashing
  slot before its deadline receives `503`.
- Session tokens are 256-bit random values, base64url-encoded. Core stores only
  their SHA-256 digest; the token itself exists only in the login response.

## Administrative API

All under `/api/admin/v1/`, requiring administrative authorization.

```text
POST /api/admin/v1/users                     {"username": "...", "password": "..."}
POST /api/admin/v1/users/{user_id}/disable   (empty body)
POST /api/admin/v1/users/{user_id}/enable    (empty body)
```

Creation returns `201` with `user_id`, normalized `username`, `status`, and
`created_at`; a taken username returns `409`. Request bodies must contain
exactly the listed string fields: unknown, duplicate, or missing fields return
`400`. Disabling a user **revokes all of that user's sessions** in the same
transaction. Re-enabling does not restore revoked sessions; the user must log in
again.

## Authentication API

```text
POST   /api/identity/v1/sessions   {"username": "...", "password": "..."}
GET    /api/identity/v1/session    Authorization: Bearer <token>
DELETE /api/identity/v1/session    Authorization: Bearer <token>
```

A successful login returns `201`:

```json
{"user_id":"user_…","token":"…","token_type":"Bearer","expires_at":"2026-10-25T19:00:00Z"}
```

- **Expiration:** sessions have an absolute lifetime of **12 hours**
  (`users.SessionTTL`). They are not extended by use. Expired sessions are
  rejected and are pruned when the same user next logs in.
- **Failing safely:** unknown usernames, wrong passwords, malformed usernames or
  passwords, and disabled accounts all return the identical
  `401 {"error":"invalid credentials"}`. Unknown or malformed usernames still
  run a full dummy argon2id verification, and account status is checked only
  after password verification, so response timing does not reveal whether an
  account exists or is disabled. Storage failures return a generic `503`.
  Passwords and tokens are never logged.
- **Disabled users:** session creation re-checks `ACTIVE` while holding a lock
  on the user row, so a user disabled between password verification and session
  creation gets no session. `GET /session` also requires the user to be
  `ACTIVE`.
- `GET /session` returns only `user_id` and `expires_at`, not the username.
  `DELETE /session` (logout) is idempotent and returns `204`.
- Responses are `Cache-Control: no-store`. Serve Core over verified TLS and do
  not log `Authorization` headers.

## Not in this phase

There is no per-account lockout or IP rate limiting yet beyond the bounded
hashing concurrency, and no password change, self-service registration, MFA,
refresh tokens, pairwise Node pseudonyms, or admin UI. Put a rate-limiting
reverse proxy in front of `/api/identity/v1/sessions` for internet exposure.

## Verification

```sh
cd core
go test ./...                                   # domain + HTTP tests
ARKNET_TEST_DATABASE_URL=... go test ./internal/postgres/   # real PostgreSQL
```

The PostgreSQL tests cover schema constraints (no plaintext passwords),
expiry, pruning, logout, revocation on disable, and a concurrent
disable-versus-login race that must never leave a usable session.
