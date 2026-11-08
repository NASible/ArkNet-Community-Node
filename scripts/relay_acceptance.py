"""Real relay acceptance evidence. Standard library, never logs packet payloads.

Capture the entire namespace (including loopback: the connector's decrypted
BitTorrent stream). Raw pcaps contain capabilities and are temporary/private.
Only counters and verdicts belong in the sanitized acceptance report.
"""
from __future__ import annotations

import ipaddress
import os
import re
import socket
import struct
import subprocess
import time
from pathlib import Path

CAPTURE_DOCKERFILE = '''FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends tcpdump iptables && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["tcpdump"]
'''


def gateway_hosts(addresses):
    """Preserve fixed gateway names when Docker removes a stopped service's DNS.

    No Node address is published: each side sees only its own gateway services.
    These are the same fixed addresses enforced by the namespace firewall.
    """
    return {'arknet-' + service: addresses[service] for service in ('core', 'tracker', 'relay')}


def read_pcap(stream):
    """Decode classic pcap Ethernet/SLL/SLL2, rejecting incomplete evidence.

    TCP checksum offload is normal in a namespace capture. IP fragmentation,
    unsupported transports, malformed lengths and snaplen truncation fail shut.
    """
    header = stream.read(24)
    if len(header) != 24:
        raise ValueError('missing pcap header')
    endian = {b'\xd4\xc3\xb2\xa1': '<', b'\xa1\xb2\xc3\xd4': '>',
              b'\x4d\x3c\xb2\xa1': '<', b'\xa1\xb2\x3c\x4d': '>'}.get(header[:4])
    if endian is None:
        raise ValueError('unsupported pcap magic')
    nano = header[:4] in (b'\x4d\x3c\xb2\xa1', b'\xa1\xb2\x3c\x4d')
    link = struct.unpack(endian + 'I', header[20:24])[0]
    offsets = {1: 14, 113: 16, 276: 20}
    if link not in offsets:
        raise ValueError('unsupported pcap link type')
    result = []
    while True:
        record = stream.read(16)
        if not record:
            break
        if len(record) != 16:
            raise ValueError('truncated pcap record')
        sec, frac, size, original = struct.unpack(endian + 'IIII', record)
        if size != original or size > 262144:
            raise ValueError('snaplen truncated packet')
        frame = stream.read(size)
        if len(frame) != size:
            raise ValueError('truncated pcap packet')
        ip = frame[offsets[link]:]
        if not ip:
            raise ValueError('missing network header')
        # ARP is not application data. Agent namespaces keep IPv6 (loopback
        # ::1, ICMPv6), so IPv6 is parsed and scanned too; anything else fails.
        ethertype = frame[12:14] if link == 1 else frame[14:16] if link == 113 else frame[:2]
        if ethertype == b'\x08\x06':
            continue
        if link != 1 and ethertype not in (b'\x08\x00', b'\x86\xdd'):
            raise ValueError('unexpected network protocol')
        if ip[0] >> 4 == 6:
            src, dst, proto, transport = _ipv6(ip)
        elif ip[0] >> 4 == 4:
            if len(ip) < 20:
                raise ValueError('truncated IPv4')
            ihl, total, fragment = (ip[0] & 15)*4, int.from_bytes(ip[2:4], 'big'), int.from_bytes(ip[6:8], 'big')
            if fragment & 0x3fff or ihl < 20 or total > len(ip):
                raise ValueError('fragmented or truncated IPv4')
            transport = ip[ihl:total]
            proto = ip[9]
            src, dst = socket.inet_ntoa(ip[12:16]), socket.inet_ntoa(ip[16:20])
        else:
            raise ValueError('unexpected network protocol')
        item = dict(time=sec + frac/(1e9 if nano else 1e6), src=src, dst=dst, proto=proto,
                    sport=0, dport=0, seq=0, flags=0, payload=b'')
        if proto == 6:
            if len(transport) < 20:
                raise ValueError('truncated TCP')
            sport, dport, seq = struct.unpack('!HHI', transport[:8])
            offset = (transport[12] >> 4)*4
            if not 20 <= offset <= len(transport):
                raise ValueError('invalid TCP offset')
            item.update(sport=sport, dport=dport, seq=seq, flags=transport[13], payload=transport[offset:])
        elif proto == 17:
            if len(transport) < 8:
                raise ValueError('truncated UDP')
            sport, dport, length = struct.unpack('!HHH', transport[:6])
            if length != len(transport):
                raise ValueError('truncated UDP payload')
            item.update(sport=sport, dport=dport, payload=transport[8:])
        elif proto in (1, 58):
            item['payload'] = transport[4:]
        else:
            raise ValueError('unsupported IP transport')
        result.append(item)
    return result


def _ipv6(ip):
    """Fixed header plus hop-by-hop/routing/destination options; no fragments."""
    if len(ip) < 40:
        raise ValueError('truncated IPv6')
    length, proto = int.from_bytes(ip[4:6], 'big'), ip[6]
    if 40 + length > len(ip):
        raise ValueError('truncated IPv6')
    body = ip[40:40 + length]
    for _ in range(8):
        if proto not in (0, 43, 60):
            break
        if len(body) < 8 or len(body) < (body[1] + 1) * 8:
            raise ValueError('truncated IPv6 extension')
        proto, body = body[0], body[(body[1] + 1) * 8:]
    if proto not in (6, 17, 58):
        raise ValueError('unsupported IPv6 transport or fragment')
    address = lambda raw: ipaddress.IPv6Address(raw).compressed
    return address(ip[8:24]), address(ip[24:40]), proto, body


def _received(packet, own_addresses):
    """Bytes delivered into this namespace. IPv6 link-local and multicast
    traffic is included: conservative, since it cannot be attributed safely."""
    if packet['dst'] in own_addresses:
        return True
    dst = ipaddress.ip_address(packet['dst'])
    return dst.version == 6 and (dst.is_multicast or dst.is_link_local or dst.is_loopback)


MEDIA_CONTEXT = 28  # bytes on each side of a binary match that must be known media


def _inside_known_content(payload, index, width, known_content):
    """True when a binary match sits inside bytes the harness itself generated.

    Compressed media is near-random, so a 4-byte address appears in ~150 MB of
    it by chance. Excuse a match only if the full window around it (the match
    plus MEDIA_CONTEXT bytes on each side) occurs verbatim in a known file;
    anything shorter or not verbatim still counts as a leak.
    """
    start, end = index - MEDIA_CONTEXT, index + width + MEDIA_CONTEXT
    if start < 0 or end > len(payload):
        return False
    window = payload[start:end]
    return any(window in content for content in known_content)


def _binary_leak(payload, needle, known_content):
    index = payload.find(needle)
    while index != -1:
        if not _inside_known_content(payload, index, len(needle), known_content):
            return True
        index = payload.find(needle, index + 1)
    return False


def inspect_capture(packets, own_addresses, counterpart_addresses, known_content=()):
    """Scan received application bytes, including TCP segment boundaries.

    Headers are not application data. Reassemble each TCP direction by sequence,
    refusing missing stream bytes instead of yielding a false absence proof.
    Scan UDP individually; TCP re-transmissions are deduplicated. No payload is
    included in errors (grants and endpoint tokens must not reach logs).

    Text addresses are refused everywhere. A binary (packed) address is excused
    only when it is a chance match inside `known_content`: the media files the
    harness generated and transferred.
    """
    if not counterpart_addresses:
        raise AssertionError('missing counterpart inventory: cannot prove absence of leaks')
    received = [p for p in packets if _received(p, own_addresses)]
    texts, packed = [], []
    for address in counterpart_addresses:
        ip = ipaddress.ip_address(address)
        texts.append(str(ip).encode())
        packed.append(ip.packed)
    payloads, stream_count = reassemble_payloads(received)
    if not payloads:
        raise AssertionError('no received application payload captured')
    for payload in payloads:
        if any(needle in payload for needle in texts) or \
                any(_binary_leak(payload, needle, known_content) for needle in packed):
            raise AssertionError('counterpart address found in received application bytes')
    return dict(packets=len(packets), received_payload_bytes=sum(map(len, payloads)),
                tcp_streams=stream_count, counterpart_addresses_checked=len(counterpart_addresses))


def reassemble_payloads(packets):
    """Recover directional TCP data without trusting packet boundaries."""
    streams, payloads, generations = {}, [], {}
    for p in packets:
        flow = p['src'], p['sport'], p['dst'], p['dport']
        if p['proto'] == 6 and p['flags'] & 2:
            generations[flow] = p['seq']
        if not p['payload']:
            continue
        if p['proto'] == 6:
            key = (*flow, generations.get(flow, None))
            streams.setdefault(key, []).append((p['seq'], p['payload']))
        else:
            payloads.append(p['payload'])
    for chunks in streams.values():
        anchor = chunks[0][0]
        chunks = [(((seq - anchor + (1 << 31)) % (1 << 32)) - (1 << 31), chunk) for seq, chunk in chunks]
        chunks.sort()
        start, data = chunks[0]
        data = bytearray(data)
        for seq, chunk in chunks[1:]:
            offset = seq - start
            if offset > len(data):
                raise AssertionError('TCP capture gap: cannot prove absence of leaks')
            overlap = min(len(chunk), len(data)-offset)
            if data[offset:offset+overlap] != chunk[:overlap]:
                raise AssertionError('conflicting TCP retransmission')
            data.extend(chunk[overlap:])
        payloads.append(bytes(data))
    return payloads, len(streams)


def assert_plaintext_peer(packets):
    """Refuse an absence proof over MSE/RC4-encrypted libtorrent loopback."""
    local_peer = [p for p in packets if p['src'] == p['dst'] == '127.0.0.1' and p['sport'] == 6881]
    payloads, _ = reassemble_payloads(local_peer)
    clear = [data for data in payloads if b'\x13BitTorrent protocol' in data and b'6:yourip' in data]
    if not clear:
        raise AssertionError('no plaintext peer handshake + BEP10 yourip; opaque/encrypted bytes cannot prove no leaks')
    return sum(map(len, clear))


def assert_no_direct_attempts(packets, own_addresses, allowed_destinations):
    """Classify every outgoing TCP SYN and UDP datagram, not just successful dials."""
    checked = 0
    for p in packets:
        attempt = p['proto'] == 17 or p['proto'] == 6 and p['flags'] & 0x12 == 2
        if not attempt:
            continue
        if ipaddress.ip_address(p['src']).version == 6:
            # No approved gateway is IPv6. Any non-loopback IPv6 dial is direct,
            # whatever (possibly link-local) source address it uses.
            checked += 1
            if not (ipaddress.ip_address(p['src']).is_loopback and ipaddress.ip_address(p['dst']).is_loopback):
                raise AssertionError('direct attempt: IPv6 connection outside loopback')
            continue
        if p['src'] not in own_addresses:
            continue
        checked += 1
        if ipaddress.ip_address(p['dst']).is_loopback:
            continue
        if (p['dst'], p['dport']) not in allowed_destinations:
            raise AssertionError('direct attempt: outgoing destination not an approved gateway')
    return checked


def assert_no_blocked_attempts(before, after):
    """tcpdump misses OUTPUT-dropped packets; require stable deny counters too.

    Conservatively include all DROP/REJECT rules, not just the OUTPUT policy.
    A counter reset/removal is not an absence proof either.
    """
    def counters(text):
        result = {}
        for line in text.splitlines():
            match = re.match(r'\[(\d+):\d+\] (.* -j (?:DROP|REJECT)(?: .*|$))', line)
            if match:
                result[match[2]] = int(match[1])
            match = re.match(r':OUTPUT (?:DROP|REJECT) \[(\d+):\d+\]', line)
            if match:
                result['OUTPUT policy'] = int(match[1])
        return result
    old, new = counters(before), counters(after)
    if not old or not new:
        raise AssertionError('no deny counters: fail-closed firewall evidence missing')
    if old != new:
        raise AssertionError('blocked egress attempt or firewall counters reset during capture')
    return 0


def assert_relay_revoked(observations, packets, relay_address, revoked_at, bound=5):
    """Join Core delivery, relay counters and closure of a live TLS socket.

    A control-channel closure alone cannot pass: the exact four-tuple must
    have carried >=128 KiB toward the receiver in the preceding three seconds.
    The harness separately proves these bytes came from A's raw LT loopback.
    """
    qualified = [o for o in observations if 0 <= o['after_seconds'] <= bound
                 and o['core_deliveries'] >= 1 and o['relay_pushes'] >= 1 and o['connections_closed'] >= 2]
    if not qualified:
        raise AssertionError('Core push and live relay closure not observed within bound')
    before, closed = {}, set()
    for p in packets:
        if p['src'] != relay_address or p['sport'] != 7443:
            continue
        key = p['src'], p['sport'], p['dst'], p['dport']
        if revoked_at - 3 <= p['time'] <= revoked_at:
            before[key] = before.get(key, 0) + len(p['payload'])
        if revoked_at <= p['time'] <= revoked_at + bound and p['flags'] & 5:
            closed.add(key)
    live_closed = [key for key in closed if before.get(key, 0) >= 128 * 1024]
    if not live_closed:
        raise AssertionError('no FIN/RST on an actively receiving relay socket within bound')
    return dict(closed_live_sockets=len(live_closed), push_observed_after=qualified[0]['after_seconds'])


def compose_override(document):
    """Use Compose 2.24.4 !override, never merge in a direct egress network."""
    lines = ['services:']
    import json
    for name, service in document['services'].items():
        lines.append('  ' + name + ':')
        for key, value in service.items():
            tag = '!override ' if key == 'networks' else ''
            lines.append('    ' + key + ': ' + tag + json.dumps(value))
    lines.append('networks: ' + json.dumps(document.get('networks', {})))
    return '\n'.join(lines) + '\n'


class Capture:
    """A tcpdump sidecar sharing exactly one real process network namespace."""
    def __init__(self, run, name, target, image, directory):
        self.run, self.name = run, name
        self.path = Path(directory) / (name + '.pcap')
        self.path.touch(mode=0o600)
        run(['docker', 'run', '-d', '--name', name, '--network', 'container:' + target,
             '--cap-drop', 'ALL', '--cap-add', 'NET_RAW', '--cap-add', 'NET_ADMIN', '--cap-add', 'DAC_OVERRIDE',
             '--cap-add', 'CHOWN', '--cap-add', 'SETUID', '--cap-add', 'SETGID',
             # Write to an anonymous volume on the VM's disk and copy out after
             # stopping: a host bind mount is too slow and makes the kernel drop.
             '-v', '/capture', image, '-i', 'any', '-n', '-s', '0', '-U', '-B', '32768',
             '-Z', 'root', '-w', '/capture/capture.pcap'], capture=True)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            logs = subprocess.run(['docker', 'logs', name], capture_output=True, text=True, check=True)
            if 'listening on' in logs.stderr:
                return
            time.sleep(.1)
        raise RuntimeError('tcpdump did not become ready')

    def stop(self):
        self.run(['docker', 'kill', '--signal', 'SIGINT', self.name], capture=True)
        self.run(['docker', 'wait', self.name], capture=True)
        self.run(['docker', 'cp', self.name + ':/capture/capture.pcap', str(self.path)], capture=True)
        logs = subprocess.run(['docker', 'logs', self.name], capture_output=True, text=True, check=True)
        # Free the VM disk at once: the stack under test needs it.
        self.run(['docker', 'rm', '-f', '-v', self.name], capture=True)
        match = re.search(r'(\d+) packets dropped by kernel', logs.stderr)
        if not match or int(match[1]) != 0:
            raise AssertionError('packet capture dropped packets or has no completion statistics')
        with self.path.open('rb') as stream:
            return read_pcap(stream)

    def firewall_counters(self):
        return self.run(['docker', 'exec', self.name, 'iptables-save', '-c'], capture=True)
