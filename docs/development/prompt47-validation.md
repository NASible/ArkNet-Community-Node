# Prompt 47 validation: terminate active transfers on revocation

Date: 2026-10-31. Branch `phase20/47-revocation-cancels-transfers`.

## Acceptance criteria

| Criterion | Evidence |
| --- | --- |
| Unit tests for cancellation on revocation | `node-agent/revocation_test.go` and `heartbeat_worker_test.go` (below); all 13 mutations of the new guards caught |
| E2E byte-counter step passes | MVP E2E run `arknet-p43-test-e5cc73079748`, item 16 PASS (all items passed) |
| ARKSEC-006 closed for cooperative Nodes; malicious window recorded under ARKSEC-004 | `docs/security-follow-ups.md`, `docs/node-revocation.md` |

## E2E step (item 16)

Node B downloads a 39.2 MB object from Core's web seed (download limit
512 KiB/s). The web seed checks the grant only when a request starts, so only
Node B's own cancellation stops these bytes. `scripts/revocation_meter.py`
samples the summed non-loopback `rx_bytes` of Node B's transfer container four
times a second. Core revokes Node B only while bytes are flowing.

| Measurement | With Prompt 47 (`arknet-p43-test-e5cc73079748`) | Negative control: Agent cancellation disabled (`arknet-p43-test-5542cab09874`) |
| --- | --- | --- |
| Node B enters `REVOKED` | 3.8 s after revocation | 1.6 s |
| Last 16 KiB block received | 0.0 s after revocation | 45.2 s after revocation |
| Bytes received in the following window | 3,757 in 52 s | — |
| Item 16 | PASS | FAIL ("did not stop within 15s") |

The bound is the heartbeat interval plus 10 s (15 s in this run; 40 s at the
30 s production default). The step also requires 45 s of silence after the
stop. That is longer than a web-seed pause: Core's write timeout ends each
response after 30 s, and libtorrent waits about 30 s before asking again. So a
Node that kept its transfer would resume within the window, which the
negative control shows.

The received-bytes counter is the kernel's, so it includes socket buffering.
The "receiving 1777 KiB/s at revocation" in the evidence is above the download
limit for that reason, and "0.0 s" means the socket buffer was already full at
revocation. The claim is the silence afterwards, not the stop time.

In the same step:

- the transfer became `CANCELLED` at 10,584,064 of 39,193,389 bytes and its
  selection `ERROR`;
- a probe announce showed the tracker no longer lists Node B in the swarm
  (1 s after the cancellation; well inside the 5-minute grant lifetime, so
  from Node B's `stopped` announce rather than grant expiry);
- the revoked Agent answered a new start with `409 node_revoked`;
- Node B's verified local media was byte-identical;
- Node A stayed `CONNECTED`.

## Tests

- Agent (`node-agent/revocation_test.go`): cancellation of downloading and
  paused transfers while keeping `AVAILABLE` files; unmatched transfers
  cancelled; finished transfers left alone; unreadable state changes nothing;
  start/resume refused (`409`/`503`) while pause/cancel work; media-miss start
  refused; reconciler woken immediately; only `REVOKED` wakes; seeds stopped.
  `heartbeat_worker_test.go`: the federation worker forwards transitions to
  extra observers (the server wiring).
- Mutation check: 13 of 13 mutations caught (revoked branch, state error,
  paused/finished filters, `ERROR` transition, wake channel, both refusal
  codes, observer condition both ways, seed state check, observer forwarding).
- Harness: `scripts/revocation_meter_test.py` (11 tests: stop inside and
  outside the bound, idle transfer, short quiet window, accumulated residual,
  pause-then-resume, recent rate, announce parsing).

## Gate

`scripts/check-all.sh --e2e`: 15 passed, 0 failed (mvp-e2e 694 s, node-upgrade
96 s, product-swarm 809 s). The gate's own E2E run (`arknet-p43-test-70701a0f9de0`)
passed item 16 again: 3,607 bytes in the 48 s after the stop, transfer stopped
at 10.7 of 39.2 MB.

## Not done

- No tracker code change: it has no channel to Core. A revoked Node's record is
  dropped at its own `stopped` announce (cooperative) or at the first announce
  after its grant expires (at most 5 minutes). Shortening that window for a
  malicious Node is ARKSEC-004.
- Core's web seed does not re-check Node status per request; an open response
  lasts up to 30 s past grant expiry (ARKSEC-004).
