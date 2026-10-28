# Functional Node Dashboard

The Node dashboard is a local, read-only operator view. Its purpose is to answer,
without a CLI:

1. Is the local Node running?
2. Is Core reachable?
3. Is this Node enrolled?
4. When did it last contact Core successfully?

## Local and federation health are separate

The dashboard deliberately presents two independent sections:

- **Local services** reports Agent and private Node database health.
- **Federation** reports enrollment, Core connectivity, and the last accepted
  outbound Core contact.

A Core outage therefore does not make the local Node look dead. The expected
outage view is local Agent/database healthy while federation is degraded or
offline. Conversely, a database failure is a local service problem and is not
reported merely as a Core connectivity problem.

## Safe status contract

`GET /api/v1/status` returns only the operator-safe read model:

```json
{
  "node_id": "node_<opaque identifier>",
  "version": "0.0.1-dev",
  "state": "CONNECTED",
  "state_changed_at": "<RFC3339 timestamp>",
  "enrollment_state": "APPROVED",
  "core_connectivity": "CONNECTED",
  "last_successful_core_contact_at": "<RFC3339 timestamp or null>",
  "database_healthy": true,
  "agent_healthy": true,
  "jellyfin_healthy": true
}
```

The response never contains the private credential seed, public-key material,
Core URL, database connection string, administrator token, signed request, raw
SQL error, hostname, MAC address, serial number, filesystem data, or user data.
The opaque Node ID is an identifier and is intentionally visible to its local
operator.

The contact timestamp is written with the Node PostgreSQL server clock only
after Core accepts an authenticated heartbeat. It is retained across Agent and
Core restarts. Failed attempts never advance it, so an offline operator can see
the last known successful contact rather than a fabricated client timestamp.

A local database failure retains the existing fail-closed HTTP 503 behavior with
a sanitized error. The UI translates that result into an actionable local
service explanation instead of displaying only an HTTP code. Likewise, a Vite
proxy 502 or network failure explains that the local Agent cannot be reached.

## Presentation

Machine states are mapped to human labels while unknown future values remain
visible rather than being silently misclassified. The interface uses semantic
status text and color together, responsive layouts, local system fonts, and no
external assets. It refreshes sequentially after each completed request, without
an overlapping request loop or loading-screen flicker.

The dashboard is observational only. It cannot enroll, approve, reset, revoke,
configure, or expose credentials.

## Verification

From repository root:

```sh
(cd node-agent && go test ./... && go test -race ./... && go vet ./...)
(cd node-ui && npm test && npm run build)
python3 scripts/check-heartbeat-stack.py
```

The disposable heartbeat/reconnect check verifies the healthy view, advances the
Node to `MESH`, stops Core, confirms that local health remains good while
federation becomes offline and the last-contact timestamp remains unchanged,
then restores Core and verifies automatic contact/timestamp recovery without an
enrollment reset. Existing development resources are not modified.
