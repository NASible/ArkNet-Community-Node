# Core Media Catalog

Phase 8 introduces the initial ArkNet-managed media catalog. Core stores catalog
metadata only; it does not scan or identify arbitrary files on community Nodes.

## Object model

Each media object contains:

- `id`: Core-generated opaque `media_` identifier with 256 random bits
- `content_type`: `VIDEO`, `AUDIO`, `IMAGE`, or `OTHER`
- `title`: human-readable title
- `size`: non-negative byte count
- `state`: `ACTIVE` or `REMOVED`
- `info_hash_v2`: empty until assigned, otherwise a lowercase 64-character
  BitTorrent v2 SHA2-256 info hash
- `created_at` and `updated_at`: RFC 3339 timestamps

Filesystem paths are never accepted as media identity or returned by the API.
Core assigns every public ID; repository callers cannot provide one.

`REMOVED` is a retained tombstone, not a database deletion. Removed objects keep
their opaque ID and metadata so Nodes can identify catalog entries that are no
longer available. A removed object cannot transition back to `ACTIVE`.

## Node API

An approved, trusted Node retrieves its catalog through node-scoped endpoints:

```text
GET /api/federation/v1/nodes/{node_id}/media
GET /api/federation/v1/nodes/{node_id}/media/{media_id}
```

Requests use the same Ed25519 node proof headers as enrollment status and
heartbeats. The signature covers the method, escaped path, empty-body digest,
timestamp, and node ID. Missing or invalid proof returns `401`; a correctly
authenticated Node that is not approved/trusted receives `403`.

The list is bounded to 100 records and includes both `ACTIVE` objects and
`REMOVED` tombstones. This initial model authorizes the catalog to approved
federation Nodes as a group; it does not introduce per-user or per-object policy.
The old global `/api/federation/v1/media` path and federation mutation routes are
not exposed.

## Node client

`node-agent/internal/catalog.Fetch` performs the signed outbound GET using the
Node's locally stored enrollment credential. It validates the bounded JSON
response, opaque IDs, media types, states, sizes, hashes, and timestamps before
returning objects. Credentials and Core URLs are not included in catalog objects.

The client is a fetch boundary only in this phase. Automatic scheduling,
caching, download planning, and swarm behavior are separate future work.

## Storage and tests

The PostgreSQL table is created by `0004_media_objects.sql`. Repository tests use
real PostgreSQL and cover Core-assigned IDs, create/read/list behavior, nullable
info-hash placeholders, removal tombstones, and terminal removal. HTTP tests
cover signed approved-node retrieval and rejection before catalog reads. Agent
tests verify signed fetching of both active and removed records.

`TestMediaCatalogPostgresHTTP` exercises creation, approved-node signed HTTP list
and lookup, and retained removal tombstones against real PostgreSQL. It also
verifies that a pending Node cannot read the catalog. Run it with
`ARKNET_TEST_DATABASE_URL` set, or use `python3 scripts/check-core-stack.py` from
repository root to run all Core tests against a disposable Compose stack.
Malformed media IDs return `400` after Node authentication, without a catalog
lookup. Creation through the Core repository defaults an omitted state to
`ACTIVE`; creation and removal are not exposed as Node mutation endpoints.
