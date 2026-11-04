#!/usr/bin/env python3
"""THROWAWAY harness for the Prompt 48 private-relay spike.

Builds the relay, two connectors and real libtorrent peers on isolated Docker
networks, then measures throughput (direct vs relayed), checks that neither
peer learns the other's address, and exercises grant expiry, revocation and
fail-closed behaviour. Prints a JSON summary; removes everything it created.

    python3 scripts/spikes/private-relay/run.py [--size-mib 128] [--runs 3]
"""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import shutil
import socket
import statistics
import subprocess
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
P = "s48"
NETS = {"wan": "10.231.0", "a": "10.232.0", "b": "10.233.0", "direct": "10.234.0"}
IMAGE = P + "-libtorrent"
MIB = 1 << 20


def sh(*cmd, check=True, timeout=900):
    r = subprocess.run(list(cmd), capture_output=True, text=True, timeout=timeout)
    if check and r.returncode:
        raise RuntimeError(f"{' '.join(cmd[:5])}: {r.stderr.strip()[-600:]}")
    return r.stdout.strip()


def wait_for(what, probe, seconds=120):
    end = time.time() + seconds
    while time.time() < end:
        value = probe()
        if value:
            return value
        time.sleep(0.5)
    raise RuntimeError("timed out: " + what)


class Spike:
    def __init__(self, work: pathlib.Path, size: int):
        self.work, self.size = work, size
        self.dirs = {name: work / name for name in ("bin", "keys", "shared", "grants", "capture", "out")}
        self.host_bin = work / "relayspike-host"

    # ------------------------------------------------------------- plumbing
    def setup(self):
        shutil.rmtree(self.work, ignore_errors=True)
        for d in self.dirs.values():
            d.mkdir(parents=True, mode=0o755)
        arch = {"aarch64": "arm64", "x86_64": "amd64"}[sh("docker", "info", "--format", "{{.Architecture}}")]
        env = dict(os.environ, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-o", str(self.host_bin), "."], cwd=HERE, check=True, env=env)
        subprocess.run(["go", "build", "-o", str(self.dirs["bin"] / "relayspike"), "."], cwd=HERE, check=True,
                       env=dict(env, GOOS="linux", GOARCH=arch))
        sh("docker", "build", "-q", "-t", IMAGE, str(HERE))
        sh(str(self.host_bin), "keygen", str(self.dirs["keys"]))
        for name, prefix in NETS.items():
            sh("docker", "network", "create", "--internal", "--subnet", prefix + ".0/24", f"{P}-{name}")

    def mounts(self, *names, ro=()):
        out = []
        for name in names:
            out += ["-v", f"{self.dirs[name]}:/{name}" + (":ro" if name in ro else "")]
        return out

    def run_detached(self, name, net, ip, image, *cmd, extra=()):
        sh("docker", "run", "-d", "--name", f"{P}-{name}", "--network", f"{P}-{net}", "--ip", ip, *extra, image, *cmd)

    def connect(self, name, net, ip):
        sh("docker", "network", "connect", "--ip", ip, f"{P}-{net}", f"{P}-{name}")

    def grant(self, label, action, ttl):
        return sh(str(self.host_bin), "grant", str(self.dirs["keys"] / "private.key"), label, self.hash, action, str(ttl))

    def admin(self, path, post=False):
        cmd = ["docker", "exec", f"{P}-relay", "wget", "-qO-"] + (["--post-data="] if post else [])
        return json.loads(sh(*cmd, "http://127.0.0.1:7080" + path))

    # ------------------------------------------------------------- topology
    def start(self):
        bin_mount = ["-v", f"{self.dirs['bin']}:/spike:ro"]
        self.run_detached("relay", "wan", "10.231.0.10", "alpine:3", "/spike/relayspike", "relay",
                          "-verify", "/keys/public.key", "-cert-out", "/shared/relay.pem",
                          extra=bin_mount + self.mounts("keys", "shared", ro=("keys",)) + ["--network-alias", "relay"])
        # libtorrent opens one listen socket per interface present at startup, so
        # attach every network before the seeder starts. The seeder's connector
        # shares its network namespace (and so its wan link) and dials libtorrent
        # on 127.0.0.1: libtorrent tells each peer the address it sees it at
        # (BEP 10 "yourip"), and loopback is the only value that reveals nothing.
        sh("docker", "create", "--name", f"{P}-seeder", "--network", f"{P}-a", "--ip", "10.232.0.20",
           "--tmpfs", f"/data:size={self.size // MIB + 64}m", "-v", f"{HERE / 'peer.py'}:/peer.py:ro",
           *self.mounts("shared"), IMAGE, "python3", "-u", "/peer.py", "seed", str(self.size))
        self.connect("seeder", "direct", "10.234.0.20")
        self.connect("seeder", "wan", "10.231.0.20")
        sh("docker", "start", f"{P}-seeder")
        info = wait_for("seeder", lambda: (self.dirs["shared"] / "obj.json").exists() and
                        json.loads((self.dirs["shared"] / "obj.json").read_text()), 300)
        self.hash = info["info_hash_v2"]
        wait_for("relay certificate", lambda: (self.dirs["shared"] / "relay.pem").exists())
        (self.dirs["grants"] / "a.txt").write_text(self.grant("node-a", "seed", 3600))
        (self.dirs["grants"] / "b.txt").write_text(self.grant("node-b", "download", 3600))
        common = bin_mount + self.mounts("shared", "grants", "capture", ro=("shared", "grants"))
        sh("docker", "run", "-d", "--name", f"{P}-conn-a", "--network", f"container:{P}-seeder", *common,
           "alpine:3", "/spike/relayspike", "listen", "-ca", "/shared/relay.pem", "-grants", "/grants/a.txt",
           "-target", "127.0.0.1:6881", "-endpoint-out", "/capture/endpoint", "-capture", "/capture/a")
        self.run_detached("conn-b", "wan", "10.231.0.31", "alpine:3", "/spike/relayspike", "socks",
                          "-ca", "/shared/relay.pem", "-grants", "/grants/b.txt", "-capture", "/capture/b", extra=common)
        self.connect("conn-b", "b", "10.233.0.31")
        self.endpoint = wait_for("endpoint", lambda: (self.dirs["capture"] / "endpoint").exists() and
                                 (self.dirs["capture"] / "endpoint").read_text())
        assert self.admin("/endpoints?hash=" + self.hash) == [self.endpoint]
        # Every address each side has. Neither may reach the other side.
        self.side_a = {"10.232.0.20", "10.234.0.20", "10.231.0.20"}
        self.side_b = {"10.231.0.31", "10.233.0.31"} | {f"10.233.0.{50 + i}" for i in range(20)}

    def download(self, name, index, relayed=True, rate=0, seconds=600, detach=False):
        net, ip = ("b", f"10.233.0.{50 + index}") if relayed else ("direct", f"10.234.0.{50 + index}")
        target, proxy = (self.endpoint, f"10.233.0.31:1080") if relayed else ("10.234.0.20:6881", "-")
        cmd = ["docker", "run", "--name", f"{P}-get-{name}", "--network", f"{P}-{net}", "--ip", ip,
               "--tmpfs", f"/dl:size={self.size // MIB + 32}m", "-v", f"{HERE / 'peer.py'}:/peer.py:ro",
               "-v", f"{self.dirs['shared']}:/shared:ro", "-v", f"{self.dirs['out']}:/out",
               IMAGE, "python3", "/peer.py", "get", name, target, proxy, str(rate), str(seconds)]
        if detach:
            sh(*cmd[:2], "-d", *cmd[2:])
            return None
        sh(*cmd, timeout=seconds + 120)
        return self.result(name)

    def result(self, name):
        sh("docker", "rm", "-f", f"{P}-get-{name}", check=False)
        return json.loads((self.dirs["out"] / f"{name}.json").read_text())

    # ------------------------------------------------------------- checks
    def throughput(self, runs):
        out = {"object_mib": self.size // MIB}
        for label, relayed in (("direct", False), ("relayed", True)):
            rates = []
            for i in range(runs):
                before = self.admin("/stats") if relayed else None
                r = self.download(f"{label}{i}", i, relayed)
                assert r["complete"], (label, i)
                rates.append(self.size / MIB / r["seconds"])
                if relayed:
                    after = self.admin("/stats")
                    moved = after.get("bytes_to_downloader", 0) - before.get("bytes_to_downloader", 0)
                    out.setdefault("relay_cpu_seconds_per_gib", []).append(
                        round((after["cpu_seconds"] - before["cpu_seconds"]) / (moved / (1 << 30)), 2))
                    self.peers_relayed = r["peers_seen"]
            out[label + "_mib_s"] = [round(x, 1) for x in rates]
            out[label + "_median_mib_s"] = round(statistics.median(rates), 1)
        # Three concurrent downloaders through one relay and one seeder connector.
        for i in range(3):
            self.download(f"parallel{i}", 10 + i, detach=True)
        for i in range(3):
            sh("docker", "wait", f"{P}-get-parallel{i}", timeout=900)
        results = [self.result(f"parallel{i}") for i in range(3)]
        assert all(r["complete"] for r in results)
        span = max(r["done_at"] for r in results) - min(r["started"] for r in results)
        out["relayed_3_parallel_aggregate_mib_s"] = round(3 * self.size / MIB / span, 1)
        out["relayed_3_parallel_each_mib_s"] = [round(self.size / MIB / r["seconds"], 1) for r in results]
        out["overhead_pct"] = round(100 * (1 - out["relayed_median_mib_s"] / out["direct_median_mib_s"]), 1)
        return out

    def leaks(self):
        # The direct baseline downloaders (10.234.0.5x) legitimately connected directly.
        baseline = {f"10.234.0.{50 + i}" for i in range(20)}
        seeder_saw = set(json.loads((self.dirs["shared"] / "seed-peers.json").read_text())) - baseline
        downloader_saw = set(self.peers_relayed)
        report = {"seeder_saw": sorted(seeder_saw), "downloader_saw": sorted(downloader_saw)}
        assert seeder_saw and seeder_saw <= {"127.0.0.1"}, seeder_saw  # only its own connector, on loopback
        assert downloader_saw == {self.endpoint.rsplit(":", 1)[0]}, downloader_saw  # only the virtual endpoint
        found = []
        for side, forbidden in (("a", self.side_b), ("b", self.side_a)):
            data = b"".join(p.read_bytes() for p in sorted(self.dirs["capture"].glob(f"{side}-*.bin")))
            report[f"captured_bytes_{side}"] = len(data)
            for ip in forbidden:
                if ip.encode() in data or socket.inet_aton(ip) in data:
                    found.append(f"{ip} reached side {side}")
        report["other_side_addresses_found"] = found
        assert not found, found
        return report

    def expiry(self, ttl=15, rate=2 * MIB):
        issued = time.time()
        (self.dirs["grants"] / "b.txt").write_text(self.grant("node-b", "download", ttl))
        before = self.admin("/stats")
        r = self.download("expiry", 5, rate=rate, seconds=ttl + 20)
        after = self.admin("/stats")
        (self.dirs["grants"] / "b.txt").write_text(self.grant("node-b", "download", 3600))
        expires = int(issued + ttl)  # whole seconds, as signed
        last = last_progress(r["timeline"])
        return {"grant_ttl_s": ttl, "last_progress_after_expiry_s": round(last - expires, 1),
                "received_mib": round(r["timeline"][-1][1] / MIB, 1), "complete": r["complete"],
                "sessions_expired": after.get("sessions_expired", 0) - before.get("sessions_expired", 0),
                "reconnects_refused": after.get("refused_connect", 0) - before.get("refused_connect", 0)}

    def revocation(self, rate=2 * MIB):
        (self.dirs["grants"] / "b.txt").write_text(self.grant("node-c", "download", 3600))
        self.download("revoke", 6, rate=rate, seconds=40, detach=True)
        wait_for("revoke download receiving", lambda: self.admin("/stats").get("sessions_active", 0) >= 1, 60)
        time.sleep(8)
        before = self.admin("/stats")
        revoked = self.admin("/revoke?label=node-c", post=True)
        sh("docker", "wait", f"{P}-get-revoke", timeout=120)
        after = self.admin("/stats")
        r = self.result("revoke")
        timeline = r["timeline"]
        at_revoke = max((rx for t, rx, _ in timeline if t <= revoked["at"]), default=0)
        (self.dirs["grants"] / "b.txt").write_text(self.grant("node-b", "download", 3600))
        return {"connections_closed": revoked["closed"],
                "last_progress_after_revoke_s": round(last_progress(timeline) - revoked["at"], 2),
                "bytes_after_revoke": timeline[-1][1] - at_revoke,
                "observed_for_s_after_revoke": round(timeline[-1][0] - revoked["at"], 1),
                "reconnects_refused": after.get("refused_connect", 0) - before.get("refused_connect", 0)}

    def seeder_revocation(self):
        self.admin("/revoke?label=node-a", post=True)
        state = wait_for("seeder connector exits", lambda: sh("docker", "inspect", "-f", "{{.State.Status}}",
                                                               f"{P}-conn-a") == "exited" and "exited", 30)
        return {"endpoints_after": self.admin("/endpoints?hash=" + self.hash), "seeder_connector": state}

    def socks_probe(self, target):
        probe = ("import socket,sys\ns=socket.create_connection(('10.233.0.31',1080),5)\n"
                 "s.sendall(b'\\x05\\x01\\x00');s.recv(2)\nip,port=sys.argv[1].rsplit(':',1)\n"
                 "s.sendall(b'\\x05\\x01\\x00\\x01'+socket.inet_aton(ip)+int(port).to_bytes(2,'big'))\n"
                 "print(s.recv(10)[1])\n")
        return int(sh("docker", "run", "--rm", "--network", f"{P}-b", IMAGE, "python3", "-c", probe, target))

    def fail_closed(self):
        out = {"socks_to_seeder_address": self.socks_probe("10.232.0.20:6881"),
               "socks_to_seeder_direct_address": self.socks_probe("10.234.0.20:6881")}
        sh("docker", "stop", "-t", "1", f"{P}-relay")
        out["socks_with_relay_down"] = self.socks_probe(self.endpoint)
        r = self.download("relaydown", 7, seconds=15)
        out["bytes_with_relay_down"] = r["timeline"][-1][1]
        assert out["socks_to_seeder_address"] == 2 and out["socks_to_seeder_direct_address"] == 2
        assert out["socks_with_relay_down"] != 0 and out["bytes_with_relay_down"] == 0
        return out

    def cleanup(self):
        names = sh("docker", "ps", "-aq", "--filter", f"name=^{P}-", check=False).split()
        if names:
            sh("docker", "rm", "-f", *names, check=False)
        for name in NETS:
            sh("docker", "network", "rm", f"{P}-{name}", check=False)
        sh("docker", "rmi", IMAGE, check=False)
        shutil.rmtree(self.work, ignore_errors=True)


def last_progress(timeline):
    last = timeline[0][0]
    for (_, prev, _), (t, rx, _) in zip(timeline, timeline[1:]):
        if rx > prev:
            last = t
    return last


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--size-mib", type=int, default=128)
    parser.add_argument("--runs", type=int, default=3)
    parser.add_argument("--work", default=os.path.expanduser("~/.hermes/cache/scratch/48/run"))
    args = parser.parse_args()
    spike = Spike(pathlib.Path(args.work), args.size_mib * MIB)
    results, status = {}, 0
    try:
        spike.cleanup()
        spike.setup()
        spike.start()
        for name, step in (("throughput", lambda: spike.throughput(args.runs)), ("addresses", spike.leaks),
                           ("grant_expiry", spike.expiry), ("revocation", spike.revocation),
                           ("seeder_revocation", spike.seeder_revocation), ("fail_closed", spike.fail_closed)):
            try:
                results[name] = step()
                print("PASS", name, json.dumps(results[name]), flush=True)
            except Exception as error:  # report and continue
                status = 1
                results[name] = {"error": f"{type(error).__name__}: {error}"[:600]}
                print("FAIL", name, results[name]["error"], flush=True)
        results["relay_stats"] = spike.admin("/stats") if sh("docker", "inspect", "-f", "{{.State.Running}}",
                                                             f"{P}-relay", check=False) == "true" else None
    finally:
        print("RESULTS", json.dumps(results), flush=True)
        spike.cleanup()
    sys.exit(status)


if __name__ == "__main__":
    main()
