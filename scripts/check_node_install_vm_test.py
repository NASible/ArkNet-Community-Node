"""Local regression checks; never start Lima or Docker."""
import importlib.util
import io
import json
from contextlib import redirect_stdout
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
spec = importlib.util.spec_from_file_location("install_vm", Path(__file__).with_name("check-node-install-vm.py"))
harness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(harness)


class InstallVMTests(unittest.TestCase):
    def test_status_uses_privileged_local_cli(self):
        with patch.object(harness, "vm", return_value=subprocess.CompletedProcess([], 0, '{"state":"ENROLLMENT_PENDING"}', "")) as vm:
            self.assertEqual(harness.agent_status(), {"state": "ENROLLMENT_PENDING"})
        command = vm.call_args.args[0]
        self.assertIn("sudo docker compose", command)
        self.assertIn("--env-file /opt/arknet/.env", command)
        self.assertIn("exec -T arknet-agent arknet-agent status --json", command)
        self.assertNotIn("curl", command)
        self.assertTrue(vm.call_args.kwargs["check"])

    def test_redacts_all_setup_codes_but_keeps_fingerprint(self):
        code = "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567"
        text = f"Operator setup code (single use):\n\n    {code}\nCertificate SHA-256 fingerprint: AA:BB\n"
        redacted = harness.redact_setup_codes(text)
        self.assertNotIn(code, redacted)
        self.assertIn("[REDACTED setup code]", redacted)
        self.assertIn("AA:BB", redacted)
        self.assertNotIn(code, harness.redact_setup_codes(f"stderr: {code}"))

    def test_management_checks_certificate_lan_https_and_loopback_http(self):
        fingerprint = ":".join(["AB"] * 32)
        output = ("Management UI (this network): https://192.168.5.15:8443\n"
                  f"  Certificate SHA-256 fingerprint: {fingerprint}\n"
                  "Operator setup code (single use, valid until tomorrow):\n\n"
                  "    ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567\n")
        bindings = {"5173/tcp": [{"HostIp": "127.0.0.1", "HostPort": "5174"}],
                    "8443/tcp": [{"HostIp": "0.0.0.0", "HostPort": "8443"}]}
        replies = [f"sha256 Fingerprint={fingerprint}\n", "200", json.dumps(bindings)]
        commands = []

        def run(command, **kwargs):
            commands.append(command)
            return subprocess.CompletedProcess([], 0, replies.pop(0), "")

        with patch.object(harness, "vm", side_effect=run), redirect_stdout(io.StringIO()):
            harness.check_management(output)
        self.assertIn("/opt/arknet/tls/cert.pem", commands[0])
        self.assertIn("openssl x509", commands[0])
        self.assertIn("--cacert /opt/arknet/tls/cert.pem", commands[1])
        self.assertIn("https://192.168.5.15:8443/", commands[1])
        self.assertNotIn("-k", commands[1])
        for command in commands:
            subprocess.run(["bash", "-n"], input=command, text=True, check=True)

        # Each security guard must fail closed on mismatches.
        for bad_output, bad_replies in [
            (output.replace("ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567", "missing"), []),
            (output.replace("192.168.5.15", "127.0.0.1"), []),
            (output, ["sha256 Fingerprint=00:11\n"]),
            (output, [f"sha256 Fingerprint={fingerprint}\n", "403"]),
            (output, [f"sha256 Fingerprint={fingerprint}\n", "200", json.dumps({
                **bindings, "5173/tcp": [{"HostIp": "0.0.0.0", "HostPort": "5174"}]})]),
            (output, [f"sha256 Fingerprint={fingerprint}\n", "200", json.dumps({
                **bindings, "8443/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8443"}]})]),
        ]:
            replies[:] = bad_replies
            with self.subTest(output=bad_output, replies=bad_replies), patch.object(harness, "vm", side_effect=run), redirect_stdout(io.StringIO()):
                with self.assertRaises(AssertionError):
                    harness.check_management(bad_output)


if __name__ == "__main__":
    unittest.main()
