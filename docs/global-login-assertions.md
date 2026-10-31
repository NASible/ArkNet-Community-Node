# Global login assertions (design)

Status: Prompt 46 (ARKSEC-001). The Core side is implemented (Prompt 46A; see
[Core implementation](#core-implementation-46a)). The Agent side, offline model
and migration are Prompt 46B and not implemented yet. The user
answered the blocking questions on 2026-10-29; see [Decisions](#decisions).

Decisions this design depends on:

- **D3:** a global user can log in offline for at most **30 days** after their
  last online verification. Core may set a shorter limit per Node or per user.
- **D4:** a global user's first login on a Node redirects to Core, so Core must
  be reachable once per Node in each offline window.

D3 was confirmed on 2026-10-29. D4 follows from the Q2 and Q7 decisions:
login happens on a Core page and returns to a callback the Node registered.

## Problem

Today a global member signs in on the Node itself
([cached global authorization](cached-global-authorization.md)):

```http
POST /api/v1/global-users/sessions
{"member_id":"member_…","password":"…"}
```

The Agent forwards the plaintext password to Core
(`POST /api/federation/v1/nodes/{node_id}/offline-authorizations`). After Core
accepts it, the Agent stores a fresh argon2id verifier of the same password
(`node-agent/internal/globalauth`) so the member can sign in during Core
outages.

The global password is reusable: it works at Core and on every Node the user
belongs to. A malicious or compromised Node sees the plaintext on every login
and keeps an offline-crackable verifier. That breaks the rule that community
Nodes are untrusted infrastructure, and it is release blocker
[ARKSEC-001](security-follow-ups.md#arksec-001).

## Goals and non-goals

Goals:

- A Node never receives, handles, or stores anything that authenticates the
  user to Core or to another Node.
- An online login produces a **one-time assertion**, signed by Core and bound
  to one Node, one pairwise member ID, one purpose, one nonce and a short
  expiry. The Node verifies it with Core's published key.
- Offline login works for up to 30 days after the last online verification
  (D3), using only material that is useless outside this Node.
- No new value that lets Nodes or Core correlate a user across Nodes.
- Migration needs no user action except one online login per Node.

Non-goals (this phase):

- Protecting a Node's *own* resources from that Node's operator. A malicious
  Node already controls its media and can let anyone in. The goal is that it
  can't reach anything **outside** itself.
- Making phishing impossible. A Node can show a fake Core login page. This
  design shrinks that risk but does not remove it (see
  [Residual risks](#residual-risks)).
- Immediate revocation while Core is unreachable. That is unchanged: entries
  last until their absolute expiry.
- Native Jellyfin-client sign-in (see [Q6](#open-questions)).

## Trust model

| Party | Trusted for |
| --- | --- |
| Core | Global identity, passwords, memberships, Node trust status, signing assertions. |
| User's browser at Core's origin | Entering the Core password. The Core login page is served only from Core's own HTTPS origin and cannot be framed (`frame-ancestors 'none'`). |
| A Node | Nothing outside itself. It may be malicious, record every byte it receives, change its clock, ignore expiry, and collude with other Nodes. |
| Network | Untrusted. Core and the Node member listener are reached over HTTPS. |

## Online flow

The flow follows D4: the user is redirected to Core. It resembles OAuth 2.0
authorization code with PKCE, but nothing is exchanged back-channel, because
Nodes connect outbound only and may be offline from Core even while the user
can reach it.

```text
Browser            Node (member listener)                Core
   │  GET /login            │                              │
   │───────────────────────>│ create pending login:        │
   │                        │  nonce N (256 bit, random)   │
   │                        │  verifier V; digest H(V)     │
   │<───────────────────────│ Set-Cookie: login state      │
   │  302 → Core /node-login?node_id&nonce=N&challenge=H(V)&redirect_uri
   │──────────────────────────────────────────────────────>│
   │                 user authenticates at Core (password, │
   │                 later passkey); Core checks           │
   │                 ACTIVE user, membership on node_id,   │
   │                 Node APPROVED + TRUSTED,              │
   │                 redirect origin registered for node   │
   │<──────────────────────────────────────────────────────│
   │  302 → https://<registered Node origin>/login/callback#assertion=A
   │  (fragment: never sent to any server or written to logs)
   │  page JS: same-origin POST {assertion, V} ───────────>│ (Node)
   │                        │ verify A and V (below)       │
   │<───────────────────────│ Node session (bearer)        │
```

1. **Start (Node).** The member listener creates a pending login with a random
   256-bit `nonce`, a random PKCE-style `verifier`, and a 5-minute expiry. It
   stores only digests, and binds the pending login to the browser with an
   HttpOnly, Secure, SameSite=Lax cookie. The member ID is **not** asked for:
   Core determines it from the signed-in user and the Node.
2. **Authenticate (Core).** The browser goes to Core's login page with
   `node_id`, `nonce` and `challenge = SHA-256(verifier)`. The user signs in
   at Core's origin by entering the global password; 46A keeps no Core
   browser session. Core requires all of the following, then issues **at
   most one** assertion per `(node_id, nonce)`:
   - valid credentials of an `ACTIVE` user with a membership on `node_id`;
   - the Node is `APPROVED` and `TRUSTED`;
   - `redirect_uri` is one of that Node's registered callbacks.

   The page names the Node by a shortened Node ID and shows the callback
   origin the browser will return to; signing in is the confirmation. Unknown
   Node, untrusted Node and unregistered callback all show one identical
   error, and none of them redirects.
3. **Return.** Core redirects only to a callback origin the Node registered in
   advance over its signed federation channel (see
   [Q2](#decisions)). The assertion travels in the URL **fragment**.
   Fragments are not sent in HTTP requests, so the assertion never appears in
   proxy, Core or Node access logs. That matches ARKSEC-007's redaction goal.
   The callback page reads the fragment, clears it with `history.replaceState`,
   and makes a **same-origin** POST with the assertion and the verifier. A
   same-origin POST is also required by the Agent's existing cross-origin
   request protection; a cross-origin form POST from Core would be rejected.
4. **Verify (Node).** The Agent accepts the assertion only if every check
   below passes, in this order, before any state changes:
   1. canonical encoding, a known version, and a key ID in Core's published
      key set;
   2. a valid Ed25519 signature;
   3. `purpose == NODE_LOGIN`;
   4. `node_id` equals this Node;
   5. `nonce` matches a pending login bound to this browser's cookie, and
      `SHA-256(verifier) == challenge`;
   6. `now < expires_at + skew` and `issued_at <= now + skew`, with a skew of
      60 s;
   7. `assertion_id` has not been seen before.

   It then atomically consumes the pending login, records `assertion_id`, saves
   the offline grant (see [Offline model](#offline-model)), and creates a Node
   session with the existing 12-hour bearer session model.

### Assertion contents

Like the [tracker tokens](swarm-authorization-tokens.md), the assertion is a
fixed-layout, versioned payload with no optional fields, encoded as
`base64url(payload) "." base64url(ed25519_signature)`. 46A defines the exact
byte offsets.

| Field | Size | Meaning |
| --- | ---: | --- |
| version | 1 | `1`. |
| purpose | 1 | `NODE_LOGIN`. Other values are reserved and rejected. |
| key ID | 8 | Which published Core assertion key signed it (rotation). |
| node ID | 32 | Raw bytes of `node_…`. The audience. |
| member ID | 32 | Raw bytes of the pairwise `member_…` for this Node. |
| nonce | 32 | Node-chosen, from step 1. |
| challenge | 32 | `SHA-256(verifier)` from step 1. |
| assertion ID | 16 | Random. Single-use at the Node. |
| issued at | 8 | Unix seconds, Core clock. |
| expires at | 8 | `issued_at + 120 s`. |
| role | 1 | `MEMBER` or `ADMIN` (membership role, as today). |
| permissions | 1 | Bit mask; `0x01 MEDIA_ACCESS` is the only bit. Unknown bits are rejected. |
| offline until | 8 | Unix seconds. At most `issued_at + 30 days` (D3), or less by Core policy. |

The signature is made over a domain-separation prefix
(`"ARKNET-NODE-LOGIN-V1\0"`) followed by the payload. The signing key is a
**dedicated** Ed25519 key. It is not the tracker key, the web-seed key, Node
enrollment keys, or TLS keys. A signature from one ArkNet purpose therefore can
never be accepted as another.

### Published verification key

Core publishes a small key set:

```http
GET /api/federation/v1/assertion-keys
```

Each entry is `{key_id, algorithm, public_key, status}`: `key_id` is the hex
of the first 8 bytes of SHA-256 of the public key, `algorithm` is `Ed25519`,
`public_key` is unpadded base64url, and `status` is `active` (the one signing
key) or `published` (listed but not signing: the next key before a rotation,
or a retired key). It is unauthenticated and cacheable
(`Cache-Control: public, max-age=3600`).

- The Agent fetches it over the same verified HTTPS connection it uses for
  federation and **persists** it, so offline login and restarts don't depend on
  Core.
- Rotation: Core publishes the new key before it signs anything with it, and
  keeps the retired key listed until every grant it signed has expired
  (≥ 30 days).
- An Agent never accepts a key that wasn't in a set fetched from its configured
  Core URL. An attacker who can make the Agent fetch a different key set
  already controls what that Node talks to, which is a Node-local compromise
  only. Per [Q4](#decisions), there is no separate pinning of the first key.

### Core implementation (46A)

| Piece | Where |
| --- | --- |
| Assertion format, signer and reference verifier | `core/internal/assertions` (byte offsets in the package comment) |
| Login page | `GET /node-login?node_id&nonce&challenge&redirect_uri` renders a server-side form; `POST /node-login` (form-encoded, same-origin only) authenticates and answers `303` to `redirect_uri#assertion=…` |
| Callback registration | `PUT /api/federation/v1/nodes/{node_id}/login-callbacks` with a signed `{"callback_urls":[…]}`, 1–4 canonical HTTPS URLs with a path (plain HTTP only for loopback); replaces the set |
| Key set | `GET /api/federation/v1/assertion-keys` |
| Storage | migration `0012_node_login.sql`: `node_login_callbacks`, and `node_login_nonces` (SHA-256 of the nonce, kept 10 minutes, pruned on the next claim) |
| Configuration | `ARKNET_ASSERTION_SIGNING_KEY_FILE` (dedicated key; refused if equal to the tracker key), `ARKNET_ASSERTION_PUBLISHED_KEYS_FILE`, `ARKNET_ASSERTION_OFFLINE_WINDOW` (≤ `720h`); overlay `deploy/core/compose.node-login.yaml`. Login is off when no key is set. |

The page sends `Content-Security-Policy: default-src 'none'; base-uri 'none';
frame-ancestors 'none'; form-action 'self' <callback origin>` (form-action also
covers the redirect after submission), plus `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer` and `Cache-Control: no-store`. The nonce is
claimed only after the password and membership checks succeed, so a failed
attempt cannot burn the user's link. Core logs only `node_login_denied` with a
reason and `node_login_assertion_issued` with the Node ID and key ID; never a
username, password, member ID, nonce, challenge or assertion.

Callback hosts must be ASCII DNS names (IDNs as punycode) or IP literals, with
a port of 1–65535 if present, because the origin is shown to the user and
written into the CSP. Accepted for now, from the 46A review:

- **HTTPS is not enforced by Core itself.** Core has no trusted-proxy
  configuration, so the page cannot tell whether a TLS proxy is in front of
  it. Deployments must serve Core over HTTPS. Enforcement and HSTS belong with
  the supported TLS ingress (ARKSEC-007, Prompt 49B).
- **A Node may register any origin, including another Node's.** The page would
  then name the wrong return address, but the assertion is still bound to the
  registering Node and every other Node rejects it.
- **Plain-HTTP loopback callbacks** are accepted for local development.

## Offline model

D3 allows offline login for at most 30 days after the last online verification.
Two options were compared.

### Option A — Core-signed, Node-bound offline grant plus a per-Node device credential (**chosen**)

- **Grant.** The verified assertion *is* the grant. Its `node_id`, `member_id`,
  `role`, `permissions` and `offline_until` are Core-signed and bound to this
  Node. The Agent keeps the assertion bytes so it can re-check the grant
  later. It never extends `offline_until` itself.
- **Device credential.** After an online login the Node issues a random
  256-bit device credential. The user's browser holds it in an HttpOnly,
  Secure, `SameSite=Strict` cookie scoped to the Node member origin; the
  Node stores only its SHA-256. It is generated by the Node, never chosen by
  the user, so it can't be a reused secret.
- **Offline login.** It is allowed only when the cause is a transport failure,
  a timeout, or a Core 5xx. All other existing fail-closed rules still apply.
  The Node then requires a stored grant with `now < offline_until`, plus a
  matching device credential, and issues a session capped at
  `min(12 h, offline_until − now)`.
- **Reconnect.** The existing signed refresh
  (`…/offline-authorizations/refresh`) runs after each accepted heartbeat. It
  can **only shorten or revoke** a grant: reduce permissions, delete it, or
  lower `offline_until`. Raising `offline_until` requires a new online login
  by the user (D3: "since the last online verification").
- **New devices.** A browser with no device credential must log in online.

| A malicious Node **can** | A malicious Node **cannot** |
| --- | --- |
| Read every grant and device-credential digest it stores, and every assertion it receives. | Use any of them at Core: Core accepts no assertion, grant or device credential as user authentication. |
| Ignore `offline_until` or the device credential, and let anyone into **its own** media. It already can. | Use them at another Node: `node_id` doesn't match, the member ID is pairwise, and the device credential is random per Node. |
| Learn when this member logs in on this Node. It already can. | Mint or extend a grant that another (honest) Node would accept. |
| Show a fake "Core" page to phish (see [Residual risks](#residual-risks)). | Learn the username, `user_…` ID, password, Core session, or the member ID on any other Node. |

### Option B — Device-bound passkey per Node

At the first online login the Node asks the browser to register a WebAuthn
passkey with the Node's origin as the relying party. Offline login then
requires a WebAuthn assertion, verified against the stored public key and the
same Core-signed grant as in Option A.

| A malicious Node **can** | A malicious Node **cannot** |
| --- | --- |
| Read the passkey public key and the signatures it receives. | Use them elsewhere: the signatures are bound to the Node's RP ID/origin, and the key pair is per-RP. |
| Ignore the check for its own media. | Use them at Core or another Node, mint grants, or learn global identity. |

Option B is stronger in theory: it is phishing-resistant per origin, and the
device secret can't be copied out of a cookie jar. It isn't chosen now because:

- WebAuthn needs a valid secure context and a registrable domain. 45B Nodes use
  self-signed LAN certificates and are often reached by IP, which browsers
  reject for WebAuthn. Option B therefore depends on the supported TLS ingress
  in ARKSEC-007 (Prompt 49B).
- Native Jellyfin and TV clients can't perform WebAuthn ceremonies.

**Choice:** ship Option A in 46B, and design the grant format so a passkey can
later replace the device cookie without changing Core ([Q5](#open-questions)).

## Replay, cross-Node, expiry and clock skew

| Threat | Handling |
| --- | --- |
| Replay of an assertion at the same Node | `assertion_id` is stored and checked once. Entries are pruned after `expires_at + skew`. The pending login is consumed in the same transaction. The verifier and the browser-bound nonce cookie mean a captured assertion fails without that browser's login state. |
| Replay at Core | Core accepts no assertion as authentication. Core also issues at most one assertion per `(node_id, nonce)`: a UNIQUE nonce digest, stored until expiry and then pruned. |
| Cross-Node use | `node_id` is the signed audience. Node B rejects anything for Node A. The member ID differs per Node anyway. Core redirects only to the callback origin registered by **that** `node_id`, so Node A never even sees an assertion for Node B. |
| Cross-purpose use | A dedicated key plus the `purpose` byte and domain-separation prefix. |
| Login CSRF / session swap (an attacker gets a victim to complete a login the attacker started) | The nonce and challenge are tied to the cookie of the browser that started the login. The Core page names the Node and asks for confirmation. Assertions are delivered only to the Node's registered origin. |
| Expiry | Assertions last 120 s. Pending logins last 5 min (Node clock only, so no skew). The offline grant ends at `offline_until`. Every check rejects exactly at its boundary (`now >= limit`). |
| Clock skew | Only the assertion's Core-clock timestamps get 60 s of skew, the same as federation request signatures. The Node rejects `issued_at > now + 60 s` and `now >= expires_at + 60 s`, so the worst-case acceptance window is 240 s. `offline_until` is 30 days away, so no skew allowance is added. During online logins the Agent compares its clock with Core's `Date` header and warns the operator above 60 s. |
| Clock rollback while offline | The Agent records the latest `issued_at` it has accepted. If its clock is earlier than that minus the skew, it treats the clock as untrustworthy and refuses offline login until it next talks to Core. A malicious Node can bypass this, but only on itself. |

## Revocation

Unchanged from [cached global authorization](cached-global-authorization.md):

- An authoritative Core denial, or a refresh saying "revoked", deletes the
  grant and device credentials and revokes the member's Node sessions.
- Disabling a user at Core also invalidates pending Core-side login state.
- While Core is unreachable, a grant lasts until `offline_until` (at most 30
  days). This is longer than today's 24-hour cache, a consequence of D3.
  Core can set shorter limits per Node or per user ([Q3](#open-questions)).
  ARKSEC-006 covers ending active transfers.

## Migration

46B performs this in one Agent migration plus a Core release:

1. **Node.** Delete every row of `cached_global_authorizations`. That removes
   each stored password-derived verifier, and `ON DELETE CASCADE` revokes all
   existing global Node sessions. Then drop the `password_hash` column and its
   argon2id CHECK, and replace the `expires_at <= validated_at + 24 hours`
   CHECK with a 30-day bound on `offline_until`. Add columns for the signed
   assertion bytes and device-credential digests. Local users and operator
   accounts are untouched.
2. **Node API.** `POST /api/v1/global-users/sessions` stops accepting a
   password. A password-shaped body gets `410 Gone` with a message that says
   to sign in through Core. The login start and callback routes take its place
   on the member listener. `GET`/`DELETE /api/v1/global-users/session` are
   unchanged.
3. **Core.** The password field of
   `POST /api/federation/v1/nodes/{node_id}/offline-authorizations` is removed,
   and the endpoint is retired once no supported Agent release calls it. The
   signed refresh endpoint stays, limited to shorten/revoke semantics.
4. **Users.** Each affected member logs in online once per Node. Nothing else
   is required. Until they do, they can't sign in offline on that Node, which
   matches today's behaviour for an expired cache.

The old verifiers can't be "converted" without the password, and keeping them
would keep the ARKSEC-001 exposure, so deleting them is the only correct
migration.

## Pairwise IDs and privacy

- The Node receives only what it already has today: its own `node_id`, the
  pairwise `member_id`, the membership role, the permissions and timestamps.
  It never receives a `user_…` ID or a username.
- Every new value is either random per login or per Node (nonce, verifier,
  assertion ID, device credential) or common to **all** Nodes (key ID,
  version, purpose). None of them is per user across Nodes, so colluding Nodes
  can't link a user's memberships with them.
- Ed25519 signatures are deterministic over unique payloads, so a signature
  reveals nothing that could be compared across Nodes.
- Core learns that the user signed in to that Node at that time. That isn't
  new: today Core sees every online login when the Node forwards the password.
  Core should keep only the nonce digest until expiry, and must not log member
  IDs, nonces, assertions or callback fragments.
- The registered callback origin tells Core how the Node's member listener is
  addressed (for example a LAN hostname). The operator chooses it; see
  [Q2](#decisions).

## Threat table

Acceptance criterion: can a malicious Node capture anything usable at Core or
at another Node? For the chosen design (online assertion plus Option A), the
answer is **no** in every row.

| # | What a malicious Node might capture | Usable at Core? | Usable at another Node? | Why |
| ---: | --- | --- | --- | --- |
| 1 | The global password | **No** | **No** | It is typed only on Core's origin. The Node never handles it, and nothing on the Node accepts it. |
| 2 | A Core session token or cookie | **No** | **No** | It is scoped to Core's origin and never sent to the Node. Core pages can't be framed. |
| 3 | A login assertion for itself | **No** | **No** | Core accepts no assertion as authentication. The audience is this `node_id`, the member ID is pairwise, and it is single-use with a 120 s life. |
| 4 | An assertion meant for another Node | **No** | **No** | Core delivers it only to the origin registered by that Node, so this Node never receives it. |
| 5 | The offline grant | **No** | **No** | It is signed for this `node_id` only, and Core does not accept it as authentication. |
| 6 | The device credential, or its digest | **No** | **No** | Random per Node and never chosen by the user. Only this Node checks it. |
| 7 | A Node database dump | **No** | **No** | No password verifiers remain after migration. The tables hold only items 5 and 6 and session digests. |
| 8 | The pairwise member ID, nonce, assertion ID, key ID | **No** | **No** | None is a credential. The values are per Node, per login, or common to all Nodes, so they can't be correlated. |
| 9 | Federation key material, its own | **No** (user-wise) | **No** | A Node's own signing key acts only as that Node. Core never issues user assertions in response to a Node request. |

What a malicious Node *can* still do is limited to itself: admit anyone to its
own media, ignore expiry and revocation for its own resources, record login
times for its own members, and attempt phishing (below).

## Residual risks

- **Phishing.** A Node can render a lookalike "Core" page and ask for the
  password. The protocol can't stop users from typing their password into the
  wrong origin. Mitigations:
  - the real flow always ends at Core's well-known origin, never inside a Node
    page;
  - the Node UI never shows a password field for global users after 46B;
  - Core rate limits and abuse controls (ARKSEC-008);
  - phishing-resistant Core login with passkeys at Core's origin
    ([Q5](#open-questions)), which would remove this risk.
- **Stolen browser cookies.** Someone with the device-credential cookie can
  log in offline on that Node until `offline_until`. They gain nothing
  elsewhere.
- **Revocation lag.** As described in [Revocation](#revocation): up to 30 days
  offline, by D3.

## Decisions

The user answered these on 2026-10-29. 46A and 46B build on them.

| # | Question | Decision |
| --- | --- | --- |
| Q1 | Confirm D3 (offline window) | **Yes: at most 30 days** on a Node since the last online verification. 30 days is the hard ceiling and the Core default; Core may set a shorter limit per Node or per user. |
| Q2 | Callback origin registration | **Recommended design.** Nodes register their member callback origin(s) with Core over the signed federation channel. Core redirects only to a registered origin for that `node_id`. Core therefore learns the Node's member URL. |
| Q4 | Key trust bootstrap | **Fetch over HTTPS from Core.** The Agent fetches the assertion key set from its configured Core URL over its verified HTTPS federation connection and persists it. Nothing is pinned in the release manifest or the enrollment response. |
| Q7 | Core login UI | **A minimal server-rendered login and confirmation page in Core** (46A), served only from Core's origin and not frameable. A separate Core web app can replace it later. |

## Open questions

These do not block 46A or 46B.

1. **Q3 — revocation versus offline length.** Is 30 days of possible
   revocation lag during a Core outage acceptable for `ADMIN`-role members, or
   should `ADMIN` get a shorter cap?
2. **Q5 — passkeys.** Should Core get passkey/WebAuthn login (removing the
   phishing risk), and should per-Node passkeys (Option B) replace the device
   cookie once ARKSEC-007's TLS ingress gives Nodes proper hostnames? Which
   prompt should own each?
3. **Q6 — non-browser clients.** How should a global member sign in to the
   Node's Jellyfin from a TV or native app? An OAuth-style device code flow
   would fit: the Node shows a code and the user approves it at Core. Is that
   in scope for Phase 20 or later?

## Not in this phase

No implementation (46A/46B). Also not covered here: PAKE, offline password
changes or recovery, native-client sign-in, per-Node passkeys, immediate
offline revocation, and Jellyfin account mapping changes.
