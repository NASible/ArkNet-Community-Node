"""Throwaway libtorrent peer for the Prompt 48 relay spike.

seed SIZE                      create a private v2 torrent of SIZE zero bytes and seed it
get NAME TARGET PROXY RATE SECS download from TARGET (ip:port), via SOCKS5 PROXY (host:port or -)
"""
import json
import os
import sys
import time

import libtorrent as lt

BASE = {"enable_dht": False, "enable_lsd": False, "enable_upnp": False, "enable_natpmp": False,
        "max_pex_peers": 0, "ignore_limits_on_local_network": False,
        # The relay carries TCP streams only; uTP would bypass the SOCKS connector.
        "enable_outgoing_utp": False, "enable_incoming_utp": False}
FLAGS = (lt.torrent_flags.disable_dht | lt.torrent_flags.disable_lsd | lt.torrent_flags.disable_pex)


def seed(size):
    os.makedirs("/data", exist_ok=True)
    with open("/data/obj", "wb") as f:
        f.truncate(size)  # zero payload: no IP address pattern can occur in it by chance
    files = lt.file_storage()
    files.add_file("obj", size)
    creator = lt.create_torrent(files, 1 << 20, lt.create_torrent.v2_only)
    creator.set_priv(True)
    lt.set_piece_hashes(creator, "/data")
    with open("/shared/obj.torrent", "wb") as f:
        f.write(lt.bencode(creator.generate()))
    info = lt.torrent_info("/shared/obj.torrent")
    # Relayed peers all arrive from the local connector's address.
    session = lt.session({**BASE, "listen_interfaces": "0.0.0.0:6881", "allow_multiple_connections_per_ip": True})
    handle = session.add_torrent({"ti": info, "save_path": "/data", "flags": FLAGS})
    while not handle.status().is_seeding:
        time.sleep(0.2)
    with open("/shared/obj.json", "w") as f:
        json.dump({"info_hash_v2": str(info.info_hashes().v2), "size": size}, f)
    seen = set()
    while True:
        seen |= {p.ip[0] for p in handle.get_peer_info()}
        with open("/shared/seed-peers.json.tmp", "w") as f:
            json.dump(sorted(seen), f)
        os.replace("/shared/seed-peers.json.tmp", "/shared/seed-peers.json")
        time.sleep(0.25)


def get(name, target, proxy, rate, seconds):
    settings = {**BASE, "listen_interfaces": "127.0.0.1:6881", "download_rate_limit": rate}
    if proxy != "-":
        host, port = proxy.rsplit(":", 1)
        settings.update(proxy_type=int(lt.proxy_type_t.socks5), proxy_hostname=host, proxy_port=int(port),
                        proxy_peer_connections=True, proxy_hostnames=True)
    session = lt.session(settings)
    handle = session.add_torrent({"ti": lt.torrent_info("/shared/obj.torrent"), "save_path": "/dl", "flags": FLAGS})
    host, port = target.rsplit(":", 1)
    timeline, seen, started, last_connect = [], set(), time.time(), 0.0
    done_at = connected = None
    while not str(handle.status().state).endswith("downloading"):
        time.sleep(0.01)
    while time.time() - started < seconds:
        status = handle.status()
        now = time.time()
        timeline.append((round(now, 3), status.total_download, status.total_wanted_done))
        seen |= {p.ip[0] for p in handle.get_peer_info()}
        if status.total_wanted_done == status.total_wanted:
            done_at = now
            break
        if status.num_peers == 0 and now - last_connect >= 2:  # stands in for tracker re-announces
            handle.connect_peer((host, int(port)))
            last_connect = now
            connected = connected or now
        time.sleep(0.05)
    first = next((t for t, rx, _ in timeline if rx > 0), None)
    # Throughput time runs from the first connection attempt, so it includes the
    # connection setup through the SOCKS connector and the relay.
    result = {"name": name, "complete": done_at is not None, "first_byte": first, "done_at": done_at,
              "started": connected, "seconds": (done_at - connected) if done_at and connected else None, "peers_seen": sorted(seen),
              "wanted": handle.status().total_wanted, "timeline": timeline}
    with open(f"/out/{name}.json", "w") as f:
        json.dump(result, f)


if __name__ == "__main__":
    if sys.argv[1] == "seed":
        seed(int(sys.argv[2]))
    else:
        get(sys.argv[2], sys.argv[3], sys.argv[4], int(sys.argv[5]), float(sys.argv[6]))
