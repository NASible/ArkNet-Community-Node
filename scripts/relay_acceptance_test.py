"""Strict, synthetic negative controls for Prompt48D capture evidence."""
import io
import socket
import struct
import unittest
from scripts import relay_acceptance as proof


def packet(payload=b'', src='127.0.0.1', dst='127.0.0.1', sport=6881, dport=40000, seq=1, flags=24):
    tcp = struct.pack('!HHIIBBHHH', sport, dport, seq, 0, 5 << 4, flags, 65535, 0, 0) + payload
    ip = struct.pack('!BBHHHBBH4s4s', 0x45, 0, 20 + len(tcp), 0, 0, 64, 6, 0,
                     socket.inet_aton(src), socket.inet_aton(dst)) + tcp
    return b'\0' * 14 + ip


def packet6(payload=b'', src='::1', dst='::1', proto=6, sport=6881, dport=40000, seq=1, flags=24, ext=b''):
    """A Linux cooked v2 (tcpdump -i any) IPv6 frame, optional extension header."""
    if proto == 6:
        body = struct.pack('!HHIIBBHHH', sport, dport, seq, 0, 5 << 4, flags, 65535, 0, 0) + payload
    elif proto == 17:
        body = struct.pack('!HHHH', sport, dport, 8 + len(payload), 0) + payload
    else:
        body = payload
    first = proto
    if ext:
        first, body = ext[0], bytes([proto]) + ext[1:] + body
    ip = struct.pack('!IHBB16s16s', 6 << 28, len(body), first, 64,
                     socket.inet_pton(socket.AF_INET6, src), socket.inet_pton(socket.AF_INET6, dst)) + body
    return b'\x86\xdd' + b'\0' * 18 + ip


def pcap(*packets, link=1):
    data = struct.pack('<IHHIIII', 0xa1b2c3d4, 2, 4, 0, 0, 262144, link)
    for i, pkt in enumerate(packets):
        data += struct.pack('<IIII', 100+i, 0, len(pkt), len(pkt)) + pkt
    return io.BytesIO(data)


class CaptureTests(unittest.TestCase):
    def test_capture_writes_to_vm_local_volume_with_large_buffer_and_copies_out(self):
        import tempfile, types
        from unittest import mock
        calls = []
        def run(command, **kwargs):
            calls.append(command)
            if command[:2] == ['docker', 'cp']:
                with open(command[3], 'wb') as out:
                    out.write(pcap(packet(b'x')).getvalue())
            return ''
        logs = types.SimpleNamespace(stderr='listening on any\n1 packet captured\n0 packets dropped by kernel\n')
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(proof.subprocess, 'run', return_value=logs):
            capture = proof.Capture(run, 'cap', 'target', 'image', directory)
            start = calls[0]
            # Anonymous volume on the VM's disk: no slow host bind-mount writes.
            mounts = [start[i + 1] for i, arg in enumerate(start) if arg == '-v']
            self.assertEqual(mounts, ['/capture'])
            self.assertEqual(start[start.index('-B') + 1], '32768')
            self.assertEqual(start[start.index('-w') + 1], '/capture/capture.pcap')
            self.assertEqual(len(capture.stop()), 1)
            self.assertIn(['docker', 'cp', 'cap:/capture/capture.pcap', str(capture.path)], calls)
            self.assertIn(['docker', 'rm', '-f', '-v', 'cap'], calls, 'capture volume must be freed on stop')

    def test_ipv6_loopback_payload_is_scanned_not_rejected(self):
        leak = b'd6:yourip4:' + socket.inet_aton('10.233.0.20') + b'e'
        packets = proof.read_pcap(pcap(packet6(leak), link=276))
        self.assertEqual(packets[0]['dst'], '::1')
        with self.assertRaisesRegex(AssertionError, 'counterpart address'):
            proof.inspect_capture(packets, {'127.0.0.1', '::1'}, {'10.233.0.20'})
        clean = proof.read_pcap(pcap(packet6(b'\0' * 64), link=276))
        self.assertEqual(proof.inspect_capture(clean, {'::1'}, {'10.233.0.20'})['received_payload_bytes'], 64)

    def test_ipv6_multicast_and_link_local_control_traffic_is_scanned(self):
        mld = packet6(b'\x8f\0\0\0' + socket.inet_aton('10.233.0.20'), src='fe80::1', dst='ff02::16',
                      proto=58, ext=b'\0\0\x05\x02\0\0\x01\0')
        packets = proof.read_pcap(pcap(mld, link=276))
        self.assertEqual(packets[0]['proto'], 58)
        with self.assertRaisesRegex(AssertionError, 'counterpart address'):
            proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})

    def test_ipv6_fragments_and_unknown_headers_fail_closed(self):
        fragment = packet6(b'x', ext=b'\x2c\0\0\0\0\0\0\x01')
        unknown = packet6(b'x', proto=99)
        for frame in (fragment, unknown):
            with self.subTest(frame=frame[:8]), self.assertRaises(ValueError):
                proof.read_pcap(pcap(frame, link=276))

    def test_ipv6_direct_attempts_are_rejected_regardless_of_source(self):
        for frame in (packet6(src='fe80::2', dst='fd00::5', flags=2), packet6(src='fe80::2', dst='fd00::5', proto=17)):
            with self.subTest(proto=frame[26]), self.assertRaisesRegex(AssertionError, 'direct attempt'):
                proof.assert_no_direct_attempts(proof.read_pcap(pcap(frame, link=276)),
                                                {'10.232.0.20'}, {('10.231.0.10', 7443)})
        loopback = proof.read_pcap(pcap(packet6(flags=2), link=276))
        self.assertEqual(proof.assert_no_direct_attempts(loopback, {'10.232.0.20'}, set()), 1)

    def test_gateway_hosts_survive_stopped_docker_dns_without_exposing_peers(self):
        addresses = {'core': '10.232.0.11', 'tracker': '10.232.0.12',
                     'relay': '10.232.0.10', 'transfer': '10.232.0.20'}
        self.assertEqual(proof.gateway_hosts(addresses), {
            'arknet-core': '10.232.0.11', 'arknet-tracker': '10.232.0.12',
            'arknet-relay': '10.232.0.10'})

    def test_split_text_and_bep10_binary_leaks_are_rejected(self):
        for payload in (b'd6:yourip4:' + socket.inet_aton('10.233.0.20') + b'e', b'10.233.0.20'):
            for split in range(1, len(payload)):
                with self.subTest(payload=payload, split=split):
                    packets = proof.read_pcap(pcap(packet(payload[:split]), packet(payload[split:], seq=1+split)))
                    with self.assertRaisesRegex(AssertionError, 'counterpart address'):
                        proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})

    def test_binary_address_by_chance_inside_known_media_bytes_is_not_a_leak(self):
        # Compressed media is near-random: any 4-byte value appears in it by chance.
        import os
        media = os.urandom(4096) + socket.inet_aton('10.233.0.20') + os.urandom(4096)
        chunk = media[3000:6000]  # one peer-wire piece slice containing the chance match
        packets = proof.read_pcap(pcap(packet(b'\x00\x00\x0b\xc1\x07' + chunk)))
        with self.assertRaisesRegex(AssertionError, 'counterpart address'):
            proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})
        result = proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'}, known_content=[media])
        self.assertEqual(result['received_payload_bytes'], 3005)

    def test_known_media_never_excuses_a_protocol_or_text_leak(self):
        import os
        address = socket.inet_aton('10.233.0.20')
        # The known media itself contains the address by chance at offset 4096.
        media = os.urandom(4096) + address + os.urandom(4096)
        chance = media[3000:6000]
        texted = media[:2000] + b'10.233.0.20' + media[2011:4000]
        leaks = {
            'BEP10 handshake next to media': b'd6:yourip4:' + address + b'e' + media[:2000],
            'chance match, then a real leak': chance + b'd6:yourip4:' + address + b'e',
            'binary address spliced into other media bytes': media[5000:6000] + address + media[6100:7000],
            'match at stream start without full context': media[4096:4096 + 40],
            'match at stream end without full context': media[4000:4100],
            'text form inside known media bytes': texted,
        }
        for name, payload in leaks.items():
            with self.subTest(name), self.assertRaisesRegex(AssertionError, 'counterpart address'):
                proof.inspect_capture(proof.read_pcap(pcap(packet(payload))), {'127.0.0.1'}, {'10.233.0.20'},
                                      known_content=[media, texted])

    def test_own_frame_address_is_not_scanned_as_application_data(self):
        packets = proof.read_pcap(pcap(packet(b'\0' * 4096, src='10.233.0.20', dst='10.232.0.20')))
        result = proof.inspect_capture(packets, {'10.232.0.20'}, {'10.233.0.20'})
        self.assertEqual(result['received_payload_bytes'], 4096)

    def test_empty_truncated_capture_cannot_pass(self):
        for data in (b'', pcap().getvalue(), pcap(packet(b'x')).getvalue()[:-1]):
            with self.subTest(size=len(data)), self.assertRaises((ValueError, AssertionError)):
                proof.inspect_capture(proof.read_pcap(io.BytesIO(data)), {'127.0.0.1'}, {'10.233.0.20'})

    def test_direct_syn_attempt_is_rejected_even_without_reply(self):
        packets = proof.read_pcap(pcap(packet(b'', src='10.232.0.20', dst='10.233.0.20', dport=6881, flags=2)))
        with self.assertRaisesRegex(AssertionError, 'direct attempt'):
            proof.assert_no_direct_attempts(packets, {'10.232.0.20'}, {('10.231.0.10', 7443)})

    def test_synack_response_to_agent_is_not_an_outgoing_dial(self):
        packets = proof.read_pcap(pcap(packet(src='10.232.0.20', dst='10.232.1.2', flags=18)))
        self.assertEqual(proof.assert_no_direct_attempts(packets, {'10.232.0.20'}, set()), 0)

    def test_sequence_wrap_still_detects_binary_leak(self):
        data = socket.inet_aton('10.233.0.20')
        packets = proof.read_pcap(pcap(packet(data[:2], seq=0xfffffffe), packet(data[2:], seq=0)))
        with self.assertRaisesRegex(AssertionError, 'counterpart address'):
            proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})

    def test_tcp_gap_never_passes(self):
        packets = proof.read_pcap(pcap(packet(b'10.233.'), packet(b'0.20', seq=100)))
        with self.assertRaisesRegex(AssertionError, 'capture gap'):
            proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})

    def test_sll_and_sll2_are_decoded(self):
        for link, header in ((113, bytes(14) + b'\x08\x00'), (276, b'\x08\x00' + bytes(18))):
            data = pcap(header + packet(b'hello')[14:]).getvalue()
            data = data[:20] + struct.pack('<I', link) + data[24:]
            packets = proof.read_pcap(io.BytesIO(data))
            self.assertEqual(packets[0]['payload'], b'hello')

    def test_override_tag_replaces_instead_of_merging_egress_network(self):
        document = {'services': {'transfer': {'networks': {'private': {}, 'gateway': {}}}},
                    'networks': {'gateway': {'external': True}}}
        rendered = proof.compose_override(document)
        self.assertIn('networks: !override {"private": {}, "gateway": {}}', rendered)
        self.assertNotIn('networks: !override {"gateway": {"external"', rendered)

    def test_firewall_denied_attempt_counters_cannot_hide_behind_pcap(self):
        before = ':OUTPUT DROP [0:0]\n[0:0] -A ARKNET -j REJECT --reject-with icmp-port-unreachable\n'
        after = ':OUTPUT DROP [0:0]\n[1:60] -A ARKNET -j REJECT --reject-with icmp-port-unreachable\n'
        with self.assertRaisesRegex(AssertionError, 'blocked egress attempt'):
            proof.assert_no_blocked_attempts(before, after)
        self.assertEqual(proof.assert_no_blocked_attempts(before, before), 0)
        with self.assertRaisesRegex(AssertionError, 'no deny counters'):
            proof.assert_no_blocked_attempts('', '')

    def test_relay_close_requires_push_counts_and_matching_active_socket(self):
        observations = [dict(after_seconds=2, core_deliveries=1, relay_pushes=1, connections_closed=2)]
        data = dict(src='10.231.0.10', sport=7443, dst='10.233.0.20', dport=40000,
                    flags=24, time=99, payload=bytes(128*1024))
        closed = dict(data, flags=1, time=102, payload=b'')
        self.assertEqual(proof.assert_relay_revoked(observations, [data, closed], '10.231.0.10', 100)['closed_live_sockets'], 1)
        for changes in ({'core_deliveries': 0}, {'relay_pushes': 0}, {'connections_closed': 0}, {'after_seconds': 5.1}):
            with self.subTest(changes=changes), self.assertRaises(AssertionError):
                proof.assert_relay_revoked([{**observations[0], **changes}], [data, closed], '10.231.0.10', 100)
        for packets in ([data], [closed], [data, dict(closed, dport=50000)], [data, dict(closed, time=106)]):
            with self.subTest(packets=len(packets)), self.assertRaises(AssertionError):
                proof.assert_relay_revoked(observations, packets, '10.231.0.10', 100)

    def test_peer_evidence_must_be_plaintext_not_opaque_encrypted_bytes(self):
        handshake = b'\x13BitTorrent protocol' + bytes(48)
        extension = b'd6:yourip4:\x7f\x00\x00\x01e'
        packets = proof.read_pcap(pcap(packet(handshake + extension + bytes(4096))))
        self.assertGreater(proof.assert_plaintext_peer(packets), 4096)
        for payload in (bytes(4096), handshake + bytes(4096)):
            with self.subTest(size=len(payload)), self.assertRaisesRegex(AssertionError, 'plaintext'):
                proof.assert_plaintext_peer(proof.read_pcap(pcap(packet(payload))))

    def test_missing_counterpart_inventory_cannot_pass(self):
        packets = proof.read_pcap(pcap(packet(b'hello')))
        with self.assertRaisesRegex(AssertionError, 'counterpart inventory'):
            proof.inspect_capture(packets, {'127.0.0.1'}, set())

    def test_reordered_retransmitted_segments_still_reject_leaks(self):
        payload = b'10.233.0.20'
        packets = proof.read_pcap(pcap(packet(payload[5:], seq=6), packet(payload[:5]), packet(payload[5:], seq=6)))
        with self.assertRaisesRegex(AssertionError, 'counterpart address'):
            proof.inspect_capture(packets, {'127.0.0.1'}, {'10.233.0.20'})

    def test_allowed_gateway_and_loopback_connections(self):
        packets = proof.read_pcap(pcap(packet(src='10.232.0.20', dst='10.231.0.10', dport=7443, flags=2),
                                       packet(flags=2)))
        self.assertEqual(proof.assert_no_direct_attempts(packets, {'10.232.0.20', '127.0.0.1'}, {('10.231.0.10', 7443)}), 2)


if __name__ == '__main__':
    unittest.main()
