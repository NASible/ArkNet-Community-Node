import json
import unittest

from scripts import operator_login as ol


class OperatorLoginTests(unittest.TestCase):
    def test_totp_matches_rfc6238(self):
        secret = __import__("base64").b32encode(b"12345678901234567890").decode().rstrip("=")
        for unix, want in ((59, "287082"), (1111111109, "081804"), (2000000000, "279037")):
            self.assertEqual(ol.totp(secret, unix), want)

    def test_setup_code_parsing_and_redaction(self):
        out = "Operator setup code (single use):\n\n    ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567\n"
        self.assertEqual(ol.setup_code(out), "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567")
        self.assertNotIn("ABCD-EFGH", ol.redact_codes(out))
        with self.assertRaises(AssertionError):
            ol.setup_code("Operator accounts exist; sign in to the management UI.")

    def test_bootstrap_runs_setup_and_confirm_and_collects_secrets(self):
        calls = []

        def request(url, method, body, headers):
            calls.append((url, method, body))
            if url.endswith("/setup"):
                return 201, {}, json.dumps({"enrollment_token": "tok", "totp_secret": "JBSWY3DPEHPK3PXP"}).encode()
            return 201, {"Set-Cookie": "__Host-arknet_operator=abc123; Path=/; Secure; HttpOnly; SameSite=Strict"}, \
                json.dumps({"recovery_codes": ["AAAAA-BBBBB"]}).encode()

        op = ol.bootstrap("http://agent", "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567", request)
        self.assertEqual(op.headers, {"Cookie": "__Host-arknet_operator=abc123"})
        self.assertEqual(calls[0][2]["setup_code"], "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567")
        self.assertEqual(calls[1][2], {"enrollment_token": "tok", "totp": ol.totp("JBSWY3DPEHPK3PXP")})
        for value in ("abc123", op.password, "JBSWY3DPEHPK3PXP", "tok", "AAAAA-BBBBB"):
            self.assertIn(value, op.secrets)

    def test_failed_setup_raises_without_secrets_in_message(self):
        request = lambda url, method, body, headers: (401, {}, b'{"error":"invalid_setup_code"}')
        with self.assertRaises(AssertionError) as caught:
            ol.bootstrap("http://agent", "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567", request)
        self.assertNotIn("ABCD", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
