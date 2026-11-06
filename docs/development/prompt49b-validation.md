# Prompt 49B validation — supported TLS ingress and grant-safe logs

## Verdict and scope

ARKSEC-007's scoped acceptance criterion is satisfied. The supported TLS
reverse proxy was started from the shipped configuration, a real short-lived
signed grant traversed its Core and tracker listeners, and the unmodified raw
stdout/stderr of proxy, Core, tracker, Agent and UI contained none of the tested
grants or sentinels. A deliberately unsafe proxy logger was detected as a
negative control; it was confined to disposable resources and removed.

This validates the pre-commit Prompt 49B tree (49A plus these changes, based on
`3953e93`; committed as 49A `6d153c5` followed by the 49B commit), alongside the
existing 49A changes. It is not the frozen-commit Phase 20 release report, a live
deployment, or a claim about other proxies/CDNs. No DNS, host trust roots,
firewalls, existing credentials or live databases were changed.

Supported recipe: [TLS ingress](../tls-ingress.md).

## Executed checks

| Check | Actual result | Evidence |
| --- | --- | --- |
| Component gate | **13 passed, 0 failed; EXIT 0** | `~/.hermes/cache/scratch/49b/component-gate/`; scaffold, hygiene, gofmt, five Go modules, UI, harness/deployment, real PostgreSQL and worker checks |
| Dedicated real-container acceptance | **PASS; exit 0**, repeated after component gate | `~/.hermes/cache/scratch/49b/ingress-redaction.log`; direct `python3 -u scripts/check-ingress-redaction.py` |
| Opt-in container test plus checker regressions | **6 tests passed, zero skips** | `ARKNET_TEST_INGRESS_DOCKER=1 python3 -m unittest discover -s scripts -p ingress_redaction_test.py -v` |
| Ingress/checker offline suite | 8 offline tests passed | Container case is deliberately skipped without opt-in; separately executed above |
| UI tests and build | **21 files / 276 tests passed; build passed** | `component-gate/node-ui.log` |
| Full scripts unit suite | 108 tests, one intentional container opt-in skip; otherwise passed | `component-gate/harness-tests.log`; skipped case executed explicitly above |
| Independent review | No remaining blocking findings | Ingress/UI review, startup-guard follow-up review and separate checker review |
| Cleanup | No remaining matching containers, networks, volumes, images or scratch directories | Parent independently enumerated the `arknet-ingress-test-` namespace after acceptance |
| Static/syntax checks | PASS | `git diff --check`, shell syntax, Compose rendering; no flagged Python dynamic execution, shell interpolation or unsafe deserialization in the new checker files |

## What the live acceptance proves

- The shipped Core/tracker/ingress Compose overlays are used, not HTTP stubs or
  a substitute proxy configuration. Core and tracker have no effective direct
  host port bindings. Agent and UI run as real services with an isolated Node
  database.
- Test-only CA verification and hostname verification are enabled. The correct
  hostname succeeds; a wrong hostname and untrusted CA fail. Plain HTTP is
  refused without a credential-bearing redirect.
- A canonical signed grant is generated after image builds to avoid expiry
  during a cold build. Core returns authorization-passed `404` for missing
  media and `403` for an invalid grant. Tracker accepts a real announce and
  returns peers without a failure reason. This is a logging/authorization
  probe, **not** a completed media-download measurement.
- Torrent, both web-seed forms, HEAD, percent-encoded paths, queries,
  `endpoint_token`, unknown/malformed paths and hostile request headers are
  exercised. Expected access-record route/status multiplicities are required.
- Live UI requests reach the Agent. Stopping the Agent produces UI `502`;
  stopping Core/tracker and replacing only their network namespaces with
  closed listeners produces ingress `502`. These are refused-connection
  fixtures, not fake HTTP backends.
- UI environment deliberately includes `DEBUG=vite:proxy`, `NODE_DEBUG=http`
  and `NODE_DEBUG_NATIVE=http`. The product startup guard prevents those raw
  request-debug sinks from being enabled.
- Both Docker stdout and stderr are captured privately before any
  sanitization, scanned for raw/encoded grant and auxiliary sentinels, and
  required to contain component output. Proxy access logs remain nonempty and
  useful. The test does not infer success from absent traffic.
- Replacing the real proxy route-log field with `$request_uri` in a disposable
  mutation causes the scanner to detect the actual logged sentinel. Cleanup
  removes the mutated instance and private captures.

## Reproduced defects and regression evidence

1. **Vite HTTP proxy errors disclosed URLs.** A real failed-proxy test was RED
   before replacing proxy error messages and metadata with fixed diagnostics.
   Forwarding positive controls retain exact URLs, methods, body and headers.
2. **Inherited debug bypassed that logger.** Review reproduced raw URLs under
   `DEBUG=vite:proxy`. The supported npm/image entrypoints now clear debug
   channels before Node initializes. Eight real-process stdout/stderr cases
   passed; removing the guard made all eight fail. Restoring raw error metadata
   also failed its dedicated regression.
3. **Read-only Nginx startup failed despite `nginx -t`.** The exact image tried
   to create default FastCGI/uWSGI/SCGI temporary directories. A regression was
   observed RED, all temporary paths were moved into the bounded writable
   tmpfs, and the shipped configuration then passed real startup/acceptance.
4. **Configuration mutations were caught.** Raw URI/request-line logging, an
   unsafe error sink, a request-derived route label and blanket access-log
   suppression each caused the targeted offline contract test to fail. The
   supported configuration was not modified during those mutation checks.

## Remaining boundaries

TLS certificate issuance, renewal automation, DNS and external logging agents
remain operator responsibilities. The recipe requires the ingress overlay to
be applied last and private keys to be root-owned mode 0600 on the Docker host.
Clients behind the proxy continue to share Core's socket-peer rate budget.
Direct Node/Vite startup that bypasses the supported guard is not the supported
runtime. Additional debug/request/body logging can invalidate the guarantee.

The current task did not rerun the long MVP E2E, upgrade, product swarm or fresh
VM installation. The prior 49A full-gate product-swarm relay-configuration gap
remains separately recorded; this component gate and 49B acceptance pass do not
turn that earlier full gate into a green release verdict.
