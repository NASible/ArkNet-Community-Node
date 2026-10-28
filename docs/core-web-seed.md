# Core HTTP web seed

ArkNet Core can provide an authorized HTTP fallback source for a single-file,
private BitTorrent v2 media object. The endpoint is deliberately an object
service, not a filesystem service: clients address an opaque ArkNet media ID and
a short-lived Core-issued swarm authorization token.

## Endpoint

```text
GET  /api/web-seed/v1/media/{media_id}/{authorization}
HEAD /api/web-seed/v1/media/{media_id}/{authorization}
GET  /api/web-seed/v1/media/{media_id}/{authorization}/{media_id}
HEAD /api/web-seed/v1/media/{media_id}/{authorization}/{media_id}
GET  /api/torrent/v1/media/{media_id}/{authorization}
```

The absolute `web_seed_url` is returned with the tracker URL when Core issues a
swarm authorization and web-seed service is enabled. It is a direct BEP 19 URL
seed for one single-file torrent; clients must not append local filenames or
filesystem paths.

The one suffixed form exists for libtorrent compatibility: when a v2 file's size
is not a multiple of the piece size, libtorrent loads it with a synthetic tail
pad file, treats the torrent as multi-file, and requests
`<web_seed_url>/<torrent name>`. ArkNet torrents are named with the opaque media
ID, so Core accepts exactly one extra segment equal to the path's `media_id`
and rejects any other suffix with `404` before reading the catalog. Without this,
non-aligned objects (most real media) never receive web-seed payload.

Core also returns `torrent_url`; the transfer worker fetches
that bounded metadata before starting because private libtorrent torrents do not
support magnet metadata exchange.

The authorization is the same canonical version 2 credential described in
[swarm authorization tokens](swarm-authorization-tokens.md). Core validates its
signature, expiration, full 32-byte v2 info hash, and `download` action. A
seed-only token cannot read content. The URL is a bearer credential and must be
sent over verified TLS, excluded from application and reverse-proxy access logs,
and kept out of persistent transfer manifests and resume data.

## Content mapping and isolation

Core maps an opaque media ID to exactly one regular file named with that ID under
the configured web-seed content root:

```text
<ARKNET_CORE_WEB_SEED_ROOT>/<media_id>
<ARKNET_CORE_WEB_SEED_ROOT>/<media_id>.torrent
```

A request never supplies a relative path. Core validates the fixed media-ID
syntax, resolves the catalog row by that exact ID, and serves content only when
the row is `ACTIVE`, its full v2 hash matches the signed authorization, and the
approved file size matches catalog metadata. Missing, removed, non-regular,
symlinked, or size-mismatched content is not served. Torrent metadata is also an
exact regular non-symlink file and is capped at 16 MiB. Directory listing and
arbitrary suffix traversal are not endpoints.

The content root is an operator-managed staging boundary. Publishing content is
therefore a two-part approval: an active Core catalog object and an exact file
under the root. Local source paths are never stored in or returned by the media
catalog API.

## Range behavior

The endpoint supports standard HTTP `Range` semantics required by libtorrent URL
seeds:

- full responses return `200 OK`;
- satisfiable byte ranges return `206 Partial Content` with `Content-Range`;
- open-ended and suffix ranges are supported;
- unsatisfiable ranges return `416 Requested Range Not Satisfiable`;
- `HEAD` returns metadata without a response body.

Responses use `application/octet-stream`, `X-Content-Type-Options: nosniff`, and
`Cache-Control: no-store`. Core seeks directly in the file rather than loading a
whole object into memory.

## Configuration

Web-seed service is disabled unless both variables are configured:

```dotenv
ARKNET_CORE_WEB_SEED_ORIGIN=https://core.example.test
ARKNET_CORE_WEB_SEED_ROOT=/srv/arknet/web-seed
```

The origin is used to build absolute URLs for transfer clients. Production
origins require HTTPS; plaintext HTTP is only a deliberate local-development
exception. The root must be an absolute, clean, non-root directory. Core tracker
signing configuration must also be enabled because web-seed authorization uses
the same narrowly scoped swarm token and dedicated key.

Mount the content root read-only into the Core container. Do not mount an entire
media library, host root, or operator home directory.

## Transfer-engine boundary

The local transfer service accepts both issued URLs only when their origin matches
the configured trusted Core web-seed origin and its path contains the same opaque
object ID. It fetches the torrent metadata without redirects or proxies, validates
its private v2 hash and size, supplies the web-seed URL to libtorrent, removes
unapproved URL/HTTP seeds, and keeps both bearer URLs in memory only.
Persisted manifests and resume data omit web-seed credentials. A refreshed
short-lived URL can replace the in-memory URL for an existing transfer.

Libtorrent's SSRF mitigation remains enabled for normal HTTPS origins. The
explicit development-only HTTP opt-in disables that library guard so a strictly
allowlisted `arknet-core`/private Docker origin can be reached; ArkNet still
rejects any other origin, encoded path, userinfo, query, fragment, or metadata-
supplied seed. Never enable the HTTP exception for an untrusted Core endpoint.

Full Core-to-Agent authorization refresh and completed-file import belong to the
end-to-end media-transfer workflow; this endpoint does not expose a global file
browser or a long-lived download credential.
