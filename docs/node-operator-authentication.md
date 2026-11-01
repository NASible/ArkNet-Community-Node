# Node Operator Authentication (design)

Status: **accounts, bootstrap, sessions, shell recovery, permission enforcement,
operator UI and LAN HTTPS listener implemented (Prompts 45A/45B).**
[ARKSEC-002](security-follow-ups.md#arksec-002) is closed by the
[Prompt 45B validation](development/prompt45b-validation.md): generated permission
and spy tests, operator-authenticated E2E/upgrade, and real Chromium HTTPS
sign-in/logout with loopback-only plain HTTP.

This design follows decision **D2** from the
[Phase 20 prompt pack](development/hermes-phase20-prompts.md), confirmed by the
user on 2026-10-27: Node-local operator accounts that work offline, with OIDC
only as an optional add-on. All open design questions were answered the same day;
see [Decisions](#decisions).

## Problem

Before Prompt 45B, every Agent management route was protected only by
`http.NewCrossOriginProtection`, which stops browser cross-site requests and
nothing else. Anyone able to send an HTTP request to the Agent could:

- create invitations and receive their secrets;
- create, disable and enable local users;
- change Peer Serving, media-miss and automatic-update settings;
- start, pause and cancel transfers, and queue or delete media;
- trigger update checks and read diagnostics.

By default the Agent (`127.0.0.1:8082`) and the UI (`127.0.0.1:5174`) are
published on loopback only. But every remote-access setup, including the
live-test's TLS ingress, makes the whole management plane reachable to anyone
who can reach that ingress.

## Goals and non-goals

Goals:

- An operator must sign in before any management action. Requests are denied
  *before* side effects.
- Works with Core unreachable and with no external identity provider.
- No default password, and no window during which management routes are open.
- Recovery needs only local shell access to the Node host.
- Operator rights can never be obtained with a global-member or local-user
  credential.
- Operator accounts never leave the Node. Core learns nothing about them.

Non-goals for 45A/45B:

- multiple roles beyond a single operator role (decision Q4);
- OIDC sign-in (an add-on, see [OIDC add-on](#optional-oidc-add-on));
- passkeys in the first implementation (decision Q2);
- abuse controls on *member* login, which belong to
  [ARKSEC-008](security-follow-ups.md#arksec-008);
- protecting Jellyfin's own administrator account, which Jellyfin manages.

## Trust model

| Actor | Trust | Basis |
| --- | --- | --- |
| Local shell user (root / `docker` group on the Node host) | Full | Can already read the database, the Agent's keys and every volume. Recovery is rooted here because it adds no new power. |
| Operator signed in through the UI | Full management | Operator session cookie. |
| Global member | Own session only | Pairwise member ID plus a password checked by Core. See [cached global authorization](cached-global-authorization.md). |
| Local user | Own session only | Node-local account. See [local users](local-users.md). |
| Anyone else who can reach the Agent | None | Only `/health` and member login routes. |
| Core | None on the management plane | Core never authenticates operators and never receives operator data. |

## Operator identity and storage

Operator accounts live only in the Agent's PostgreSQL database, in new tables
created by a forward-only migration. Nothing is copied into heartbeats,
enrollment, the catalog or diagnostics bundles.

```text
operators
  id                 bigint identity          -- internal
  public_id          'op_' + 64 lowercase hex -- 256 random bits, unrelated to username
  username           normalized like local users, unique
  status             ACTIVE | DISABLED
  password_hash      argon2id PHC string      -- same parameters and concurrency pool as local users
  totp_secret        20 random bytes          -- see "TOTP secret at rest"
  totp_last_step     bigint                   -- rejects replay of an accepted code
  credentials_changed_at, created_at, updated_at

operator_recovery_codes
  operator_id, code_digest (SHA-256), used_at  -- 10 per enrollment, single use

operator_sessions
  token_digest (SHA-256 of 32 random bytes), operator_id,
  created_at, authenticated_at, last_seen_at, idle_expires_at, expires_at

operator_setup_codes
  code_digest (SHA-256), purpose SETUP | RECOVERY, operator_id NULL for SETUP,
  created_at, expires_at, used_at

operator_events                                -- local audit trail, never sent to Core
  at, operator_id NULL, event, outcome         -- no secrets, no passwords, no codes
```

Operator IDs use a separate `op_` prefix, and member and operator tokens are
looked up in separate tables. A member token presented as an operator cookie
therefore finds nothing.

### Credential types

The first implementation (45A) uses **password plus TOTP**, both mandatory.

- **Password:** argon2id with the existing local-user length policy (no
  composition rules).
- **TOTP:** RFC 6238 with SHA-1, 6 digits and a 30-second step, accepting ±1
  step. Each accepted step is stored, so a code cannot be used twice. It is
  implemented with the Go standard library (HMAC-SHA1), so no new dependency.
- **Recovery codes:** 10 single-use codes of 10 random base32 characters each,
  shown once at enrollment and stored only as SHA-256 digests. They replace TOTP,
  never the password.

Passkeys (WebAuthn) are the stronger option in D2, but they need a secure
context and a stable relying-party ID (a hostname). Many community Nodes will be
reached by LAN IP, or over `http://localhost` through an SSH tunnel.
Password+TOTP works in every case, so it comes first and passkeys are an
optional second credential type later (decision Q2).

### TOTP secret at rest

A TOTP secret has to be readable to verify codes, so unlike a password it cannot
be hashed. It is stored in the Agent database. Encrypting it with a key kept on
the same host would not stop anyone who can read the database, and that person
is the local shell user, who already has full trust. The secret is never logged,
never returned after enrollment, and never included in backups made by ArkNet
tooling unless the database itself is backed up.

## First-operator bootstrap

There is no default account and no default password. Operator routes without a
session return `401 {"error":"operator_required"}` without consulting any
repository. Routes are never open, not even briefly. On a new Node, the printed
setup code and the sign-in page's **Set up operator** action begin enrollment.

1. **Issue a setup code from the local shell.**

   ```text
   sudo arknet-node operator setup-code
   ```

   This runs `docker compose exec -T arknet-agent arknet-agent operator setup-code`.
   It is a separate short-lived process inside the Agent container. It stores
   the SHA-256 digest of a fresh code with a **1-hour** expiry, invalidates any
   earlier unused SETUP code, and prints the code to the invoking terminal only.
   The running Agent server never sees the code, so it cannot appear in the
   Agent's log stream. 45A adds a log-capture test for this.
   - Code format: 20 random bytes as base32 in groups, e.g.
     `ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567`.
   - `install.sh` runs this step at the end of a fresh install and prints the
     setup URL and code. The upgrade path (45B) does the same for an existing
     Node without operators.
2. **Begin setup in the UI.** The operator opens the setup page and submits
   `setup_code`, `username` and `password`. In one transaction the Agent:
   - consumes the code;
   - creates a PENDING enrollment bound to a 15-minute enrollment token;
   - returns the TOTP provisioning URI (also shown as a QR code).
3. **Confirm setup.** The operator submits the enrollment token and a current
   TOTP code. The account becomes ACTIVE, the 10 recovery codes are shown once,
   and an operator session starts.

If step 3 never happens, the enrollment expires and step 1 must be repeated.
Every code issue, consumption and expiry is recorded in `operator_events`.

Further operators are added the same way. A signed-in operator can also invite
one from the UI with a fresh login (see [Step-up](#step-up-re-authentication)).

## Sessions

| Property | Value |
| --- | --- |
| Token | 32 random bytes, sent only in a cookie, stored as SHA-256 digest |
| Cookie | `__Host-arknet_operator`, `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/`, no `Domain` |
| Idle timeout | 30 minutes, refreshed by authenticated requests |
| Absolute lifetime | 12 hours from sign-in, no extension |
| Concurrent sessions | at most 10 per operator; the oldest is revoked first |
| Logout | `DELETE` of the session deletes the row and expires the cookie |
| Revoked on | logout, password or TOTP change (all *other* sessions), disable, shell recovery (all sessions) |

`Secure` cookies are accepted over HTTPS and on `http://localhost`, so the LAN
HTTPS listener, remote ingress and SSH tunnels all work. Plain HTTP on a LAN IP
is not supported for operator sign-in; the plain-HTTP listener stays
loopback-only. Browsers' handling of `__Host-` cookies on `http://localhost`
must be confirmed in the 45B UI test. If one does not accept it, the fallback
name is `arknet_operator` with the same flags.

The Agent sees the browser's origin only through the UI proxy (`/api` is proxied
same-origin), so the cookie belongs to the UI origin. The UI keeps no operator
data in browser storage. It learns the signed-in state from
`GET /api/v1/operator/session`.

### LAN HTTPS listener

Decision Q3: every Node ships a self-signed HTTPS listener for the management UI,
so operators on the LAN never send a password or session cookie in clear text.

- **Where:** the `arknet-ui` container terminates TLS and keeps proxying `/api`
  to the Agent over the internal `control` network. The default port is `8443`,
  bound to all interfaces and set with `ARKNET_UI_HTTPS_BIND`. The existing
  plain-HTTP UI port stays on `127.0.0.1` for SSH tunnels.
- **Certificate:** `install.sh` (and the upgrade path, for existing Nodes)
  generates a per-Node ECDSA P-256 key and a self-signed certificate valid for
  825 days. The SANs cover the host name, `localhost` and the host's
  non-loopback addresses at install time. The files live in the Node's state
  directory: the key is mode 0600, readable only by the UI container, and never
  leaves the host or reaches Core.
- **First-use trust:** the browser warns about the self-signed certificate. So
  `install.sh` and `arknet-node operator setup-code` print the certificate's
  SHA-256 fingerprint next to the setup code. The operator compares it with
  the browser's certificate details before accepting it.
- **Rotation:** `sudo arknet-node tls renew` reissues the certificate (for
  example after an address change) and prints the new fingerprint. A
  certificate is never replaced silently.
- **Not covered:** operators who already run their own reverse proxy with a
  real certificate can keep using it. The self-signed listener is the default,
  not a requirement.
- **Hardening:** TLS 1.2 minimum, HSTS off (a self-signed host must not pin
  itself), and the same security headers as the current UI.

### Login

- `POST /api/v1/operator/sessions` takes `username`, `password` and either a
  `totp` or a `recovery_code` value.
- Failures for an unknown user, wrong password, wrong second factor or disabled
  account get the same `401` body. An unknown user still runs a dummy argon2id
  verification.
- **Abuse control:**
  - After 5 consecutive failures for a username, a growing delay applies (1 s
    doubling to 15 min), counted in the database. The delay never locks the
    account permanently, because recovery is local anyway.
  - A global limit of 10 failed operator logins per minute protects the hashing
    pool.

### Step-up re-authentication

Some actions require that the session's `authenticated_at` is less than 10
minutes old. Otherwise they return `401 {"error":"reauthentication_required"}`:

- managing operators;
- changing your own password or TOTP;
- regenerating recovery codes;
- creating an invitation, because the response carries an invitation secret.

### CSRF

1. `SameSite=Strict` keeps the cookie off every cross-site request.
2. The existing `http.NewCrossOriginProtection` stays on every state-changing
   route. It rejects browser requests whose `Sec-Fetch-Site` or `Origin` shows a
   cross-origin source.
3. Operator authentication reads **only** the cookie, never an `Authorization`
   header. A request with both is rejected, so a member bearer token can never
   be confused with operator credentials.

A non-browser client never carries the cookie unless it stole it, and stealing
the cookie is out of CSRF scope. No separate CSRF token is needed.

## Permission matrix

Legend:

- **public**: no credential.
- **member**: the caller's own global-member or local-user credential, checked
  by the route itself; it never confers operator rights.
- **operator**: an operator session.
- **operator+step-up**: an operator session authenticated in the last 10 minutes.

In the first implementation every ACTIVE operator holds the single `operator`
role (decision Q4). Every route registered in `node-agent` today appears below. The
script in [Matrix check](#matrix-check) verifies this.

| Route | Permission | Notes |
| --- | --- | --- |
| `GET /health` | public | Liveness only. It must stay free of state, version and connectivity detail. |
| `GET /api/v1/status` | operator | Core connectivity, release and health state help fingerprinting. `arknet-node` stops calling it over HTTP (see below). |
| `GET /api/v1/diagnostics` | operator | Registered only when diagnostics are enabled. |
| `GET /api/v1/media` | operator | |
| `POST /api/v1/media/{id}/selection` | operator | |
| `DELETE /api/v1/media/{id}/retained` | operator | |
| `GET /api/v1/transfers` | operator | |
| `POST /api/v1/media/{id}/transfer/{action}` | operator | |
| `POST /api/v1/invitations` | operator+step-up | The response returns the invitation secret. |
| `GET /api/v1/settings/peer-serving` | operator | |
| `PUT /api/v1/settings/peer-serving` | operator | |
| `GET /api/v1/settings/media-miss-policy` | operator | |
| `PUT /api/v1/settings/media-miss-policy` | operator | |
| `GET /api/v1/media-misses` | operator | |
| `POST /api/v1/media-misses/{id}/ignore` | operator | |
| `GET /api/v1/updates` | operator | |
| `POST /api/v1/updates/check` | operator | |
| `GET /api/v1/settings/automatic-updates` | operator | |
| `PUT /api/v1/settings/automatic-updates` | operator | |
| `GET /api/v1/local-users` | operator | Lists Node-local accounts only. |
| `POST /api/v1/local-users` | operator | |
| `POST /api/v1/local-users/{id}/disable` | operator | |
| `POST /api/v1/local-users/{id}/enable` | operator | |
| `POST /api/v1/local-users/sessions` | public | Member login with the local user's own credential (ARKSEC-008 covers abuse). |
| `GET /api/v1/local-users/session` | member | The caller's own bearer session. |
| `DELETE /api/v1/local-users/session` | member | |
| `POST /api/v1/global-users/sessions` | public | Retired password login; always `410 Gone` ([ARKSEC-001](security-follow-ups.md#arksec-001), Prompt 46B). |
| `POST /api/v1/global-users/login` | public | Starts a Core-bound member login, or an offline login while Core is unreachable ([global login assertions](global-login-assertions.md)). |
| `POST /api/v1/global-users/login/complete` | public | Verifies Core's one-time assertion and creates a member session. |
| `GET /api/v1/global-users/session` | member | |
| `DELETE /api/v1/global-users/session` | member | |
| `POST /api/v1/operator/setup` | public | Requires a valid setup or recovery code from the local shell. |
| `POST /api/v1/operator/setup/confirm` | public | Requires the enrollment token from `/setup` and a current TOTP code. |
| `POST /api/v1/operator/sessions` | public | Operator login: password plus TOTP or one recovery code. |
| `GET /api/v1/operator/session` | operator | Reads only the operator cookie. |
| `DELETE /api/v1/operator/session` | operator | Logout; idempotent. |

Planned routes, not registered yet. They join the table above when they land,
and 45B's generated test then enforces them:

| Planned route | Permission |
| --- | --- |
| `PUT /api/v1/operator/password`, `PUT /api/v1/operator/totp`, `POST /api/v1/operator/recovery-codes` | operator+step-up |
| `GET /api/v1/operators`, `POST /api/v1/operators/{id}/disable` | operator+step-up |

Until then, list and disable operators with `arknet-node operator list|disable`.

Enforcement (45B):

- The check sits in one middleware at registration time, not inside handlers,
  and runs before body parsing or any repository call.
- A route registered without a matrix entry fails the generated test.
- Denials return `401` without a session and `403` with a session that lacks
  the permission, using a uniform body that never names the missing permission
  beyond `operator_required`.

### Listener split

45C serves `public` and `member` routes, including `/health`, on the member
listener (`ARKNET_MEMBER_ADDR`, container `:8082`, host `127.0.0.1:8086`).
`operator` and `operator+step-up` routes stay on the management listener
(`ARKNET_AGENT_ADDR`, container `:8081`, host `127.0.0.1:8082`). A route on
the wrong listener answers `404`. The partition is derived from this matrix,
and a test generated from the registered routes checks both directions. Both
host ports are loopback-only; the LAN HTTPS UI from 45B is the only other
entry point.

### Status for local tooling

`arknet-node` reads `GET /api/v1/status` over HTTP from inside the Agent
container to decide whether an upgrade is healthy. After 45B that call would be
denied. 45A therefore adds a local CLI, `arknet-agent status --json`. It returns
the same document and runs through `docker compose exec`, the same local-shell
trust root as recovery. `arknet-node` switches to it.

The remote checks (live-test `verify-deployment.py`, the E2E and upgrade
harnesses) sign in as an operator instead. Trusting a source IP was rejected:
Docker's port publishing and the UI proxy make the source address an unreliable
signal.

## Recovery using local shell access only

| Situation | Command (local shell on the Node host) | Effect |
| --- | --- | --- |
| Lost TOTP device | (no shell needed) sign in with a recovery code | Then re-enroll TOTP; the used code is spent. |
| Lost password, or TOTP and all recovery codes | `sudo arknet-node operator recover NAME` | Revokes all of NAME's sessions **immediately**. Issues a 1-hour RECOVERY code bound to NAME. Redeeming it in the UI sets a new password and TOTP and issues new recovery codes. |
| Every operator lost or unknown | `sudo arknet-node operator list`, then `operator recover NAME` or `operator setup-code --additional` | `list` prints usernames, status and last sign-in, never secrets. `--additional` issues a SETUP code even though operators exist. |
| Suspected compromise of an operator | `sudo arknet-node operator disable NAME` | Disables the account and revokes its sessions. Works with the Agent's web interface unreachable. |

All recovery actions are written to `operator_events`, and the UI shows them on
the next sign-in ("recovered from local shell at …"). Recovery never touches Core,
and it works offline.

## How operator sessions differ from member sessions

| | Operator session | Global-member session | Local-user session |
| --- | --- | --- | --- |
| Who | Node operator | Global user, by pairwise member ID | Node-local account |
| Verified by | Agent (password + TOTP) | Core online, or cached verifier offline | Agent (password) |
| Transport | `__Host-` cookie only | `Authorization: Bearer` only | `Authorization: Bearer` only |
| Storage | `operator_sessions` | global session table | `local_user_sessions` |
| Grants | management routes in the matrix | that member's own session routes | that user's own session routes |
| Leaves the Node | never | member ID to Core for verification | never |

Rules 45B tests:

1. Operator middleware ignores `Authorization` and rejects requests that carry
   both an operator cookie and an `Authorization` header.
2. A valid global-member or local-user token, presented as a bearer or as the
   cookie value, gets `401` on every operator route, and no repository is called.
3. Creating a local user, or a global member joining by invitation, never
   creates or modifies an operator.
4. A person can hold both an operator account and a member account. They are
   unrelated records with different credentials. Signing in as one never signs
   in the other.

## Optional OIDC add-on

Out of scope for 45A/45B. When added:

- an operator account may link one OIDC subject (issuer plus `sub`) from an
  operator-configured IdP such as Authentik;
- OIDC sign-in then substitutes for password+TOTP for that account;
- linking requires step-up;
- at least one operator must keep local credentials, so a down IdP never locks
  the Node, and shell recovery always works.

The Node never depends on Core or on another person's IdP for operator access.

## Matrix check

The route column was checked against the source with
`~/.hermes/cache/scratch/operator-matrix/check-route-matrix.py` (kept out of the
tree because this prompt makes no code changes; 45B turns it into the generated
test). The script:

- scans every non-test `.go` file under `node-agent/` for `Handle` and
  `HandleFunc` registrations;
- expands patterns built in `for … range map[…]{…}` loops;
- fails on any registration whose pattern it cannot resolve;
- compares the resulting set with the first column of the matrix above.

Result on 2026-10-27, commit `d26f335`: 29 registered routes and 29 matrix rows,
no missing or extra entries. After Prompt 45A: 34 routes and 34 rows.

## Decisions

The user answered the design questions on 2026-10-27. 45A, 45B and 45C build on
these answers.

| # | Question | Decision |
| --- | --- | --- |
| Q1 | Confirm D2: Node-local operator accounts, OIDC optional | **Yes.** |
| Q2 | Passkeys from the start? | **No.** Password plus TOTP is enough for now; passkeys may be added later as an optional credential. |
| Q3 | Plain HTTP on the LAN | **Nodes ship a self-signed HTTPS listener.** See [LAN HTTPS listener](#lan-https-listener); built in 45B. |
| Q4 | Roles | **One `operator` role** for now. |
| Q5 | Timeouts (30 min idle, 12 h absolute, 10 min step-up) | **Yes.** |
| Q6 | Separate member listener | **Yes.** Built in 45C; see [Listener split](#listener-split). |
| Q7 | `arknet-agent status` CLI instead of HTTP status for `arknet-node` | **Yes.** |
| Q8 | 1-hour setup-code lifetime | **Yes.** |
