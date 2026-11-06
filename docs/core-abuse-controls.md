# Core login and enrollment abuse controls

Prompt 49A policy (decision D10): abuse controls apply to Core, not the
independently operated Node-local login services.

## Rate budgets

Core's identity session API (`POST /api/identity/v1/sessions`) and its browser
login (`POST /node-login`) share a token-bucket budget. Every attempt counts,
including malformed requests and successful logins; budgets do not depend on
whether a username exists or is active.

| Operation | Per source IP | Global |
| --- | --- | --- |
| Password login (both endpoints combined) | 10/minute; burst 5 | 60/minute; burst 10 |
| Enrollment submission | 10/minute; burst 5 | 60/minute; burst 10 |
| Enrollment status polling | 120/minute; burst 30 | 1,200/minute; burst 120 |

Each budget has its own bounded map of at most 4,096 source addresses. Idle
entries can be reclaimed after 10 minutes. If state is full, an unfamiliar
source is refused rather than evicting an active source and resetting its
budget. Admission charges the per-source and global buckets atomically.

Excess requests get `429`, `Retry-After` (whole seconds) and
`Cache-Control: no-store`, before password verification, signature verification
or database writes. Health checks and ordinary authenticated operations do not
consume these budgets. No account-specific lockout permits an attacker to lock
out a username globally.

The source is the socket peer IP (IPv4-mapped IPv6 is normalized). Forwarding
headers are ignored. Behind an ingress proxy, clients therefore share that
proxy's budget; do not loosen this by trusting arbitrary `X-Forwarded-For`.
Trusted-proxy identity handling is not implemented here.

The limiter is process-local: restarting Core resets rate state, and multiple
Core replicas would multiply the effective budget. This is a bounded
single-Core control, not distributed denial-of-service protection. Protect
public ingress and provision resources accordingly. Source addresses are held
only in bounded memory and are not added to application logs.

## Signed enrollment and privacy

The unsigned three-field compatibility path is removed. Registration requires
exactly `node_id`, `version`, empty `capabilities`, and canonical base64
`public_key`, plus the timestamp and Ed25519 signature headers from the
[enrollment handshake](node-enrollment-handshake.md). The signature covers the
method, escaped path, exact body digest, timestamp and Node ID.

Valid signed submissions receive the same two-field `200` receipt with the
submitted Node ID and `enrollment_status: PENDING`, regardless of whether the
ID is new, pending, already decided, key-conflicting or cannot be admitted at
quota. **This is a receipt, not proof that a pending record exists.** It never
changes an approved, rejected or revoked record. A caller must prove possession
of the pinned key to read actual status. Unknown IDs and incorrect-key status
queries have the same `401` response. Storage failures remain generic `503`s;
rate limits remain state-independent `429`s.

## Pending capacity and expiry

The confirmed policy caps pending records at 1,000, with a fixed 7-day lifetime
from initial creation. Retries do not extend that lifetime. Quota admission and
expiry handling must serialize in PostgreSQL so concurrent submissions cannot
oversubscribe the cap. Expired pending records must not be approved or exposed
as live pending records. Approved, rejected and revoked records never expire
through this mechanism.

The expiry clock is the database clock. A generic receipt at full quota does
not guarantee later admission; operators should monitor pending capacity and
handle requests rather than repeatedly recreating the database.
