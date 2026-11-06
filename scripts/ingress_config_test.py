"""Offline safety contract for the supported grant-safe TLS ingress."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]


class IngressConfigurationTests(unittest.TestCase):
    def test_acceptance_check_is_part_of_e2e_gate(self):
        gate = (ROOT / "scripts/check-all.sh").read_text()
        self.assertTrue("step ingress-redaction python3 -u scripts/check-ingress-redaction.py" in gate,
                        "E2E gate must execute the TLS log-redaction acceptance check")

    def test_read_only_image_uses_writable_temporary_paths(self):
        source = (ROOT / "deploy/core/ingress/nginx.conf").read_text()
        for kind in ("client_body", "proxy", "fastcgi", "uwsgi", "scgi"):
            self.assertTrue(f"{kind}_temp_path /tmp/" in source,
                            f"{kind} temporary storage must work on a read-only image")

    def test_access_log_uses_only_allowlisted_fields(self):
        config = ROOT / "deploy/core/ingress/nginx.conf"
        self.assertTrue(config.is_file(), "supported TLS ingress configuration is missing")
        source = config.read_text()
        formats = re.findall(r"log_format\s+\w+\s+(.*?);", source, re.S)
        self.assertTrue(formats, "redacted access logging must remain enabled")
        allowed = {"arknet_log_route", "status", "body_bytes_sent", "request_time"}
        for definition in formats:
            self.assertLessEqual(set(re.findall(r"\$(\w+)", definition)), allowed)
        self.assertIn("access_log /dev/stdout arknet_safe", source)
        error_sinks = re.findall(r"^\s*error_log\s+([^;]+);", source, re.M)
        self.assertTrue(error_sinks)
        self.assertTrue(all(sink.strip() == "/dev/null" for sink in error_sinks),
                        "Nginx request-error sinks cannot safely redact URLs")
        routes = re.search(r"map\s+\$uri\s+\$arknet_log_route\s*\{(.*?)\}", source, re.S)
        if routes is None:
            self.fail("route labels must come from the fixed allowlist")
        labels = re.findall(r'"([^"\n]*)"', routes.group(1))
        self.assertTrue(labels)
        self.assertTrue(all("$" not in label for label in labels), "request fragments must not enter labels")
        self.assertNotIn("access_log off", source)


if __name__ == "__main__":
    unittest.main()
