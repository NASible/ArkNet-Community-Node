# Supported Core and tracker TLS ingress

Prompt 49B / ARKSEC-007 supplies a Docker Compose Nginx overlay with TLS-only
listeners and grant-safe access logs. It protects the URL capabilities in:

- tracker: `/announce/<grant>`;
- Core torrent metadata: `/api/torrent/v1/media/<id>/<grant>`;
- Core web seed: `/api/web-seed/v1/media/<id>/<grant>[/<name>]`.

It is optional for loopback development, but is the supported TLS termination
recipe when publishing these services. It does not deploy DNS, open a firewall,
issue certificates, or alter any live environment automatically.

## Configuration

Requires Docker Compose **2.24.4+**, including its `!reset` support. The overlay
is `deploy/core/compose.ingress.yaml`; the shipped proxy configuration is
`deploy/core/ingress/nginx.conf`. The Nginx image is pinned to a multi-platform
stable image digest rather than silently following a mutable tag.

Provide a trusted certificate-chain PEM and matching private-key PEM outside the
repository. The certificate's SANs must match the Core and tracker DNS names
clients actually use. They may use the same hostname with different ports, or
different hostnames covered by the certificate. For private PKI, explicitly
install only its public CA on participating clients; never disable verification
or distribute the CA private key. Public-CA issuance/renewal is operator-managed.

Keep the private key mode `0600`, owned by root on the Docker host. The Nginx
master reads it before workers drop to the `nginx` account. Only the privilege
transition capabilities are retained; the container filesystem is read-only,
with bounded temporary storage. Certificate/key mounts are read-only and refuse
to auto-create missing host files.

Set these variables in your existing protected deployment environment (do not
replace an existing `.env` or commit credentials):

| Variable | Meaning / default |
| --- | --- |
| `ARKNET_INGRESS_TLS_CERT_FILE` | Required absolute host path to certificate chain PEM. |
| `ARKNET_INGRESS_TLS_KEY_FILE` | Required absolute host path to matching private key PEM. |
| `ARKNET_CORE_HTTPS_BIND` | Host IP/port for Core; `127.0.0.1:8443` by default. |
| `ARKNET_TRACKER_HTTPS_BIND` | Host IP/port for tracker; `127.0.0.1:8444` by default. |
| `ARKNET_TRACKER_ANNOUNCE_URL` | Externally reachable HTTPS tracker origin, e.g. `https://tracker.example.net:8444`. |
| `ARKNET_CORE_WEB_SEED_ORIGIN` | When web seeds are enabled, externally reachable HTTPS Core origin, e.g. `https://core.example.net:8443`. |

The loopback bind defaults intentionally do not publish a new public service.
To expose it, explicitly choose an approved host address (for example
`0.0.0.0:8443` and `0.0.0.0:8444`) and configure the corresponding firewall and
DNS records. To use port 443 for both, publish on **two distinct host IPs**.
This recipe uses separate TLS sockets, not hostname-dependent upstream routing.
There is no cleartext HTTP listener or HTTP-to-HTTPS redirect to carry a grant in
an insecure URL or a redirect log.

Use the existing tracker signing, verification, relay endpoint verification,
and web-seed settings described in [Core deployment](../deploy/core/README.md).
This overlay does not make those settings optional or bypass relay security.
For example, from `deploy/core`, with all required settings already supplied:

```sh
docker compose -f compose.yaml -f compose.tracker.yaml \
  -f compose.web-seed.yaml -f compose.ingress.yaml \
  up --build -d --wait
```

Include any needed login/relay overlays **before** `compose.ingress.yaml`.
Apply the ingress overlay last: it resets the direct Core and tracker host-port
publications rather than leaving an unprotected bypass listener. Do not add
later port overrides. PostgreSQL remains unpublished. The proxy reaches
backends over Docker networks; backends must not be separately exposed by a
host firewall rule, container override or another proxy.

Health checks test configuration validity; they do **not** establish that a
certificate is trusted, DNS works, or upstreams are reachable. Verify the actual
TLS endpoints using the real hostname and client trust roots. For an isolated
private-CA check, `curl --cacert <public-ca.pem> https://<hostname>:<port>/health`
is appropriate; `curl -k` is not verification.

After renewal, replace the matching certificate/key files, then recreate only
`arknet-ingress` with the same Compose files and `--force-recreate --no-deps`.
Individual file bind mounts can retain old inodes after atomic host replacement;
a reload alone is not a reliable certificate-rotation procedure. Verify the
served certificate and health after recreation before removing the prior files.

## Log contract

Nginx emits nonempty JSON access records containing only:

- a fixed route label;
- response status;
- response bytes;
- request duration.

Grant-bearing paths become fixed labels such as
`/announce/[REDACTED]` or `/api/web-seed/v1/media/[REDACTED]`. Unknown paths become
`[REDACTED]`; ordinary APIs become `/api/[REDACTED]`. No substring from a request
path is copied into a log field. Neither raw/escaped URLs, queries, method
strings, Host, Referer, User-Agent, cookies, authorization, nor arbitrary headers
are logged. Tokens therefore remain absent even on unknown routes, malformed
requests, or upstream failure. Proxy forwarding is not rewritten to achieve
redaction: authorization and request signatures still see the original request.

**Nginx's built-in error log cannot redact request URLs.** Its request-time sink
is explicitly `/dev/null`, not stderr and not merely a higher severity level.
Use safe access status/duration counters, container health and backend
structured lifecycle diagnostics for operations. Do not enable debug/error
request logging on this ingress to troubleshoot a live grant; reproduce with a
disposable nonsecret probe instead. Startup/configuration validation still
returns failure status.

The Node UI retains nonsecret proxy-failure diagnostics without including the
proxied URL or raw error object. The shipped image's production server
(`server.mjs`, Prompt 51) logs no request lines at all; the Vite development
server used by `npm run dev`/`preview` redacts its proxy errors. Disabling Nginx
access logs alone does not protect those messages. Supported UI npm/image
entrypoints also clear `DEBUG`, `NODE_DEBUG` and `NODE_DEBUG_NATIVE` before Node
starts, because those channels bypass any application logger. Use
`npm run dev`, `npm run serve`, `npm run preview` or the image's unchanged
default command; direct Node/Vite invocation bypasses that guard.

If another load balancer, CDN, WAF, tunnel, packet recorder or observability
agent sits in front of this ingress, it is a separate log boundary. Configure
it to the same or stricter policy before sending grants through it. This recipe
makes no claim about third-party logs, browser history or deliberately malicious
operators. TLS protects transport; redaction protects the configured log sinks.

Forwarding headers supplied by clients do not become log fields. Core's
process-local abuse controls still use the socket peer, so clients share the
proxy's budget; this change does not silently introduce trusted-proxy IP logic.

## Automated acceptance

Run from the repository root:

```sh
python3 scripts/check-ingress-redaction.py
```

The check uses only uniquely named disposable Docker resources, a test-only CA
trusted explicitly by its client, real Core/tracker/Agent/UI services, and a
short-lived sentinel grant. It exercises normal authorization, rejected and
unknown paths, and upstream failures. It checks raw container logs for the
sentinel before displaying any evidence, demands positive traffic/log controls,
and verifies a deliberately unsafe logger is detected. It must fail rather than
report absence when a required log source or route was not exercised. It does
not read developer `.env` files or change system trust, DNS, live services or
application databases.
