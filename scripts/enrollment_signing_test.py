"""Focused offline tests; never start disposable stacks or contact Core."""
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

from scripts import enrollment_signing as signing


class EnrollmentSigningTests(unittest.TestCase):
    def test_signed_wire_and_stable_per_node_key(self):
        with signing.EnrollmentSigner(interval=0) as signer:
            first = signer.request("http://localhost/api/federation/v1/enrollments", "node_a", "v1")
            second = signer.request("http://localhost/api/federation/v1/enrollments", "node_a", "v2")
            other = signer.request("http://localhost/api/federation/v1/enrollments", "node_b", "v1")
            body = json.loads(first.data)
            self.assertEqual(set(body), {"node_id", "version", "capabilities", "public_key"})
            self.assertEqual(body["capabilities"], {})
            public = base64.b64decode(body["public_key"], validate=True)
            self.assertEqual(len(public), 32)
            self.assertEqual(json.loads(second.data)["public_key"], body["public_key"])
            self.assertNotEqual(json.loads(other.data)["public_key"], body["public_key"])
            timestamp = first.get_header("X-arknet-node-timestamp")
            self.assertTrue(timestamp.isdigit())
            message = ("ARKNET-NODE-V1\nPOST\n/api/federation/v1/enrollments\n" +
                       hashlib.sha256(first.data).hexdigest() + "\n" + timestamp + "\nnode_a").encode()
            signature = base64.b64decode(first.get_header("X-arknet-node-signature"), validate=True)
            with tempfile.TemporaryDirectory(dir=signing.temporary_root()) as directory:
                root = Path(directory)
                # Ed25519 SubjectPublicKeyInfo DER prefix (RFC 8410).
                (root / "public.der").write_bytes(bytes.fromhex("302a300506032b6570032100") + public)
                (root / "message").write_bytes(message)
                (root / "signature").write_bytes(signature)
                command = [signer.openssl, "pkeyutl", "-verify", "-pubin", "-keyform", "DER",
                           "-inkey", str(root / "public.der"), "-rawin", "-in", str(root / "message"),
                           "-sigfile", str(root / "signature")]
                self.assertEqual(subprocess.run(command, capture_output=True).returncode, 0)
                (root / "message").write_bytes(message + b"tampered")
                self.assertNotEqual(subprocess.run(command, capture_output=True).returncode, 0)

    def test_private_material_is_restrictive_and_removed_on_failure(self):
        signer = signing.EnrollmentSigner(interval=0)
        run = subprocess.run
        transient = []
        private_material = []

        def inspect_command(command, **kwargs):
            if "-sign" in command:
                key = Path(command[command.index("-inkey") + 1])
                transient.append(key.parent)
                self.assertEqual(key.stat().st_mode & 0o777, 0o600)
                self.assertEqual(key.parent.stat().st_mode & 0o777, 0o700)
                self.assertEqual(key.parent.parent, Path(signing.temporary_root()))
                private_material.append(key.read_bytes())
                self.assertNotIn("PRIVATE KEY", " ".join(command))
                # Even OpenSSL diagnostics containing key data must be suppressed.
                return subprocess.CompletedProcess(command, 1, b"", key.read_bytes())
            return run(command, **kwargs)

        with signer, mock.patch.object(signing.subprocess, "run", side_effect=inspect_command):
            with self.assertRaises(RuntimeError) as error:
                signer.request("http://localhost/enroll", "node_a", "v1")
        self.assertEqual(len(transient), 1)
        self.assertFalse(transient[0].exists())
        self.assertEqual(signer._keys, {})
        self.assertFalse(private_material[0].decode() in str(error.exception))

    def test_paces_every_attempt_before_timestamp(self):
        clock = [100.0]
        sleeps = []

        def sleep(seconds):
            sleeps.append(seconds)
            clock[0] += seconds

        with mock.patch.object(signing.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(signing.time, "sleep", side_effect=sleep), \
                mock.patch.object(signing.time, "time", side_effect=lambda: clock[0]):
            with signing.EnrollmentSigner() as signer:
                first = signer.request("http://localhost/enroll", "node_a", "v1")
                second = signer.request("http://localhost/enroll", "node_a", "v1", {"wan_ip": "bad"})
        self.assertEqual(sleeps, [7.5])
        self.assertEqual(first.get_header("X-arknet-node-timestamp"), "100")
        self.assertEqual(second.get_header("X-arknet-node-timestamp"), "107")


if __name__ == "__main__":
    unittest.main()
