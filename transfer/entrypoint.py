#!/usr/bin/python3
"""Install namespace-local default-deny egress, then irrevocably drop privilege.

Only configured tracker/web-seed/relay TCP destinations are reachable outside
loopback. The libtorrent listener and relay connector share loopback. No DNS
or peer-supplied IP can widen this boot-time allowlist; DNS changes need restart.
"""
import ipaddress
import os
import re
import socket
import subprocess
import sys
from urllib.parse import urlsplit

HOST = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?\Z")


def hostport(text):
    parsed = urlsplit("tcp://" + text)
    if (not parsed.hostname or parsed.username is not None or parsed.password is not None or
            parsed.path or parsed.query or parsed.fragment or parsed.port is None or not 1 <= parsed.port <= 65535):
        raise ValueError("invalid egress destination")
    host = parsed.hostname
    try:
        ipaddress.ip_address(host)
    except ValueError:
        if not HOST.fullmatch(host):
            raise ValueError("invalid egress hostname") from None
    return host, parsed.port


def configured_targets(env):
    targets = set()
    for key in ("ARKNET_TRANSFER_TRACKER_ORIGIN", "ARKNET_TRANSFER_WEB_SEED_ORIGIN"):
        origin = env.get(key, "")
        if not origin:
            continue
        parsed = urlsplit(origin)
        if (parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username is not None or
                parsed.password is not None or parsed.path or parsed.query or parsed.fragment):
            raise ValueError("invalid egress origin")
        port = parsed.port or (443 if parsed.scheme == "https" else 80)
        host = "[" + parsed.hostname + "]" if ":" in parsed.hostname else parsed.hostname
        targets.add(hostport(host + ":" + str(port)))
    relays = env.get("ARKNET_TRANSFER_RELAY_EGRESS", "")
    if relays:
        entries = relays.split(",")
        if len(entries) > 16:
            raise ValueError("too many relay destinations")
        for entry in entries:
            targets.add(hostport(entry))
    return targets


def destinations(env):
    allowed = set()
    for host, port in configured_targets(env):
        for family, _, _, _, address in socket.getaddrinfo(host, port, type=socket.SOCK_STREAM):
            if family in {socket.AF_INET, socket.AF_INET6}:
                allowed.add((str(ipaddress.ip_address(address[0])), port))
    if len(allowed) > 128:
        raise ValueError("too many egress addresses")
    return allowed


def firewall_rules(allowed, family):
    version = 4 if family == socket.AF_INET else 6
    lines = ["*filter", ":INPUT DROP [0:0]", ":FORWARD DROP [0:0]", ":OUTPUT DROP [0:0]",
             "-A INPUT -i lo -j ACCEPT", "-A OUTPUT -o lo -j ACCEPT",
             "-A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
             "-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
             "-A INPUT -p tcp --dport 8084 -j ACCEPT"]
    for address, port in sorted(allowed):
        if ipaddress.ip_address(address).version == version:
            lines.append(f"-A OUTPUT -d {address} -p tcp --dport {port} -j ACCEPT")
    lines.extend(["COMMIT", ""])
    return "\n".join(lines)


def main():
    if os.geteuid() != 0:
        raise RuntimeError("firewall initialization requires startup privilege")
    allowed = destinations(os.environ)
    # Resolve before locking down. Docker's loopback resolver remains reachable;
    # arbitrary external resolver traffic is intentionally not allowed.
    for program, family in (("iptables-restore", socket.AF_INET), ("ip6tables-restore", socket.AF_INET6)):
        subprocess.run([program, "--wait", "5"], input=firewall_rules(allowed, family).encode(),
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.execvp("setpriv", ["setpriv", "--reuid=10001", "--regid=10001", "--clear-groups",
                         "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs",
                         "/usr/local/bin/arknet-transfer", *sys.argv[1:]])


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # No destination, credential, URL or raw OS error crosses the log boundary.
        print('{"event":"transfer_network_initialization_failed"}', file=sys.stderr, flush=True)
        sys.exit(1)
