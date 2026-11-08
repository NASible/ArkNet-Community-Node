"""Prompt 51B: gate logs are scanned before they become public CI artifacts."""
import importlib.util
import io
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("check_log_secrets", HERE / "check-log-secrets.py")
scanner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scanner)

CAPABILITY = "A" * 40 + "." + "b" * 40
SAMPLES = {
    "capability": f"grant {CAPABILITY} issued",
    "admin-token": "token arknet_adm_" + "x" * 30,
    "private-key": "-----BEGIN PRIVATE KEY-----",
    "setup-code": "code ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567",
    "database-password": "postgres://arknet:s3cretPassw0rd@postgres:5432/arknet",
}
CLEAN = """STEP go-core PASS (6s)
postgres://postgres:***@127.0.0.1:5432/postgres?sslmode=disable
ghcr.io/nasible/arknet-agent:0.1.0@sha256:f931cf5e7c2ddbd22a5a0c2d0844e28884cfdbed2ab64299641b228d77ff2ef6
integrity sha512-9QqZbCZqrXb3oPBlqLRNPh4RZyFfnnmxKHZpYb3ZSRo1XtRnR2q0e2v0xQ==
[SETUP-CODE] [REDACTED] node_75223c9c468992b24ba87f8ff2ca030549e1597408997df5bb5b9eeb7e52b613
"""


class LogSecretScanTests(unittest.TestCase):
    def scan(self, text, name="step.log"):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / "nested").mkdir()
            (Path(directory) / "nested" / name).write_text(text)
            out = io.StringIO()
            with redirect_stdout(out):
                code = scanner.main([directory])
            return code, out.getvalue()

    def test_each_secret_kind_is_found_and_never_echoed(self):
        for kind, text in SAMPLES.items():
            with self.subTest(kind=kind):
                code, output = self.scan(text)
                self.assertEqual(code, 1)
                self.assertIn(f"nested/step.log: {kind}", output)
                self.assertNotIn(text.split()[-1], output, "the secret itself must not be printed")

    def test_clean_gate_output_passes(self):
        code, output = self.scan(CLEAN)
        self.assertEqual((code, output.strip().splitlines()[-1]), (0, "no secrets found in 1 file(s)"))

    def test_json_and_binary_files_are_scanned_too(self):
        self.assertEqual(self.scan('{"token": "%s"}' % CAPABILITY, "e2e.json")[0], 1)
        self.assertEqual(self.scan(b"\x00\xff".decode("latin-1") + SAMPLES["private-key"], "bundle.bin")[0], 1)

    def test_missing_directory_fails(self):
        with redirect_stdout(io.StringIO()):
            self.assertEqual(scanner.main(["/nonexistent/arknet-logs"]), 2)


if __name__ == "__main__":
    unittest.main()
