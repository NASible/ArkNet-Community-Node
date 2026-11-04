# Private relay spike (Prompt 48) — throwaway

Proof of concept for [the private relay design](../../../docs/private-relay-design.md).
**Not product code**: not built, tested or shipped by `scripts/check-all.sh`, and
not to be extended. The product implementation will be written separately
after the design is accepted.

| File | What it is |
| --- | --- |
| `main.go` | `relayspike`: test key and grant tool (tracker grant format), the relay, the SOCKS5 downloader connector and the seeder ("listen") connector |
| `peer.py` | A real libtorrent 2.0 seeder or downloader |
| `Dockerfile` | Ubuntu 24.04 + `python3-libtorrent` for the peers |
| `run.py` | The harness: networks, containers, measurements, checks and cleanup |

Run it (Docker and Go required; about 6 minutes):

```sh
python3 scripts/spikes/private-relay/run.py --size-mib 128 --runs 3
```

It prints `PASS`/`FAIL` for these checks:

- throughput, direct and relayed;
- addresses each peer saw, and the captured relay bytes searched for the other
  side's addresses;
- grant expiry;
- revocation of a downloader and of a seeder;
- fail-closed behaviour.

It ends with a `RESULTS` JSON line, then removes every container, network,
image and scratch file it created (all named `s48-*`). It uses the subnets
`10.231.0.0/24` to `10.234.0.0/24`.
