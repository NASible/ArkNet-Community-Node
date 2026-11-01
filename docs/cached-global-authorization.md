# Global member login on a Node

An ArkNet global member signs in to a Node through ArkNet Core. The global
password is typed only on Core's own page and never reaches the Node. Core
returns a one-time assertion bound to that Node, and the Node verifies it with
Core's published key. If Core is unreachable later, the member can still sign
in on that Node for a bounded time (at most 30 days, decision D3) from a
browser that has signed in online before.

This replaced the earlier design (Node-relayed password plus a Node-held
password verifier) in Prompt 46B, closing
[ARKSEC-001](security-follow-ups.md#arksec-001). The protocol, threat table and
offline-model comparison are in
[global login assertions](global-login-assertions.md).

## Online login

```text
browser ──POST /api/v1/global-users/login──────────────▶ Node member listener
        ◀── 200 {"mode":"redirect","authorize_url":…} + login-state cookie
browser ──GET /node-login?node_id&nonce&challenge&redirect_uri──▶ Core
        (user enters username and password on Core's page)
        ◀── 303 <registered callback>#assertion=…
browser ──POST /api/v1/global-users/login/complete {"assertion":…}──▶ Node
        ◀── 201 {member_id, kind: "GLOBAL", token, expires_at} + device cookie
```

1. **Start.** The Agent fetches Core's key set (which also shows whether Core
   is reachable), then creates a pending login: a random 256-bit nonce and a
   random 256-bit login state held by the browser in the HttpOnly
   `__Host-arknet_global_login` cookie (5 minutes, `SameSite=Lax`). The Node
   stores only the nonce and `SHA-256(state)`, which is the challenge sent to
   Core. Pending logins are capped at 1,000.
2. **Core.** Core checks the password, the membership on this Node, and that
   the Node is approved and trusted, then redirects only to a callback URL the
   Node registered over its signed federation channel. The assertion travels in
   the URL fragment, so it never appears in any HTTP request log.
3. **Complete.** The member UI (`/login/callback` in `node-ui`) reads the
   fragment, removes it from the address bar, and posts the assertion. The
   Agent verifies, before changing any state: canonical encoding, a key from
   Core's published set, the Ed25519 signature, purpose, this Node as the
   audience, the nonce and challenge of *this browser's* pending login, and
   the time window (120 s lifetime plus 60 s skew). If the key ID is unknown it
   re-reads Core's key set once (key rotation). Then, in one transaction, it
   consumes the pending login, records the assertion ID (single use), stores
   the grant, issues a device credential, and creates the session.

The member is identified only by the pairwise `member_id` inside the
Core-signed assertion. The Node never learns the username or the Core
`user_…` ID.

## Offline login

`POST /api/v1/global-users/login` creates an offline session instead of a
redirect only when **all** of these hold:

- the key-set fetch failed because Core is unreachable (transport error,
  timeout or 5xx). Malformed or unexpected Core answers fail closed;
- the browser presents a device credential (`__Host-arknet_global_device`,
  HttpOnly, `SameSite=Strict`) issued by this Node at an online login. The
  Node stores only its SHA-256; at most 10 are kept per member;
- the stored grant for that member has `offline_until` in the future;
- the Node's clock is not earlier than the newest accepted `issued_at` minus
  60 s (clock rollback);
- the stored assertion still verifies with a persisted Core key and names this
  Node.

A browser without a device credential gets `503
global_authentication_unavailable`; a device credential with no valid grant
gets `401 offline_login_rejected`. When Core is reachable a device credential
never bypasses Core.

## What the Node stores

| Table | Contents |
| --- | --- |
| `cached_global_authorizations` | pairwise `member_id`, this Node's ID, role, `MEDIA_ACCESS`, `issued_at`, `offline_until` (≤ `issued_at` + 30 days, enforced by the schema), and the verified Core-signed assertion (the grant). No password, verifier, username or Core user ID. |
| `global_device_credentials` | SHA-256 of each device credential and its member. |
| `global_login_pending` | challenge (`SHA-256` of the browser's login state) and nonce, for at most 5 minutes. |
| `global_login_assertions_used` | assertion IDs until they can no longer be accepted. |
| `core_assertion_keys` | Core's published key set, as last fetched from the configured Core URL. |
| `cached_global_sessions` | SHA-256 of session tokens; a session ends at the earlier of 12 hours and `offline_until`. |

Migration `0012_global_login_assertions.sql` deleted every stored
password-derived verifier (and, by cascade, every global session) and dropped
the `password_hash` column. Affected members sign in online once.

## Reconnect refresh

After each accepted heartbeat the Agent sends the signed refresh

```http
POST /api/federation/v1/nodes/{node_id}/offline-authorizations/refresh
```

for the pairwise IDs it holds grants for. Core answers per member with
`authorized` and, if authorized, `expires_at` = now plus Core's offline window.
The Agent applies it **shorten-only**: it deletes revoked members (with their
device credentials and sessions), lowers `offline_until` when Core's answer is
earlier, and lowers `ADMIN` to `MEMBER`; it never extends a grant or raises a
role. Any change ends the member's existing sessions. Extending offline access
needs a new online login (D3: "since the last online verification").

The former `POST …/offline-authorizations` (password relay) now answers
`410 Gone` at Core, and the Node's `POST /api/v1/global-users/sessions`
answers `410 global_password_login_removed`.

## Configuration

| Agent variable | Meaning |
| --- | --- |
| `ARKNET_GLOBAL_LOGIN_CALLBACK_URLS` | Comma-separated member UI callback URLs (1–4), for example `https://node.lan:8443/login/callback`. HTTPS with a path; plain HTTP only for loopback. Empty disables global login. The Agent registers them with Core after each start. |
| `ARKNET_CORE_LOGIN_URL` | Core base URL that browsers use, when it differs from `ARKNET_CORE_URL` (for example when the Agent reaches Core over a private network). |

Core needs the node-login overlay (`ARKNET_ASSERTION_SIGNING_KEY_FILE`, see
`deploy/core/README.md`). `ARKNET_ASSERTION_OFFLINE_WINDOW` sets the offline
window (default and maximum `720h`).

## Local session API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/global-users/login` | Start a Core-bound login, or an offline login while Core is unreachable. |
| `POST` | `/api/v1/global-users/login/complete` | Verify Core's assertion and create a session. |
| `GET` | `/api/v1/global-users/session` | Resolve an unexpired Node global session (bearer token). |
| `DELETE` | `/api/v1/global-users/session` | Delete the current Node global session. |
| `POST` | `/api/v1/global-users/sessions` | Retired password login; always `410`. |

All are on the member listener and answer `Cache-Control: no-store`. The POST
routes use the Agent's cross-origin protection. Offline sessions grant only
`MEDIA_ACCESS`; membership administration, invitations and account changes
stay Core-only.

## Verification

See [Prompt 46B validation](development/prompt46b-validation.md): Agent unit
and PostgreSQL tests, the Core/Agent golden assertion, and MVP E2E item 13
(online login, offline login within the window, rejection after it, replay and
cross-Node rejection, and the malicious-Node harness).
