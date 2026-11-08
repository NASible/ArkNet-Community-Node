"""Offline regression tests for the real-container ingress acceptance checker."""
import importlib.util

import harness_scratch
from pathlib import Path
import unittest
import urllib.parse

SPEC = importlib.util.spec_from_file_location("ingress_check", Path(__file__).with_name("check-ingress-redaction.py"))


class RedactionTests(unittest.TestCase):
    def load(self):
        assert SPEC is not None and SPEC.loader is not None
        self.assertTrue(SPEC.origin and Path(SPEC.origin).exists(), "acceptance checker is missing")
        module = importlib.util.module_from_spec(SPEC)
        assert SPEC is not None and SPEC.loader is not None
        SPEC.loader.exec_module(module)
        return module

    def test_raw_and_percent_encoded_leaks_fail_without_echoing_secret(self):
        checker = self.load()
        secret = "sentinel+/token.secret"
        for value in (secret, urllib.parse.quote(secret, safe=""), ''.join('%%%02x' % b for b in secret.encode())):
            with self.subTest(encoding=len(value)):
                with self.assertRaises(checker.CheckFailure) as caught:
                    checker.scan_sink("ingress", ("request=" + value).encode(), [secret])
                self.assertEqual(str(caught.exception), "sensitive data in sink: ingress")
                self.assertNotIn(secret, str(caught.exception))
        checker.scan_sink("ingress", b'{"route":"web_seed","status":404}\n', [secret])

    def test_empty_sinks_and_missing_access_outcomes_are_not_a_pass(self):
        checker = self.load()
        with self.assertRaises(checker.CheckFailure):
            checker.require_access(b'', [('torrent', 403)])
        log = b'{"route":"torrent","status":403}\n'
        checker.require_access(log, [('torrent', 403)])
        with self.assertRaises(checker.CheckFailure):
            checker.require_access(log, [('torrent', 403), ('torrent', 403)])
        with self.assertRaises(checker.CheckFailure):
            checker.require_access(log, [('torrent', 502)])

    def test_child_failures_and_timeouts_never_echo_argv_or_output(self):
        import sys
        checker = self.load()
        for program, timeout in [("import sys; print(sys.argv[1]); sys.exit(2)", 5),
                                 ("import time; time.sleep(5)", 0.05)]:
            with self.assertRaises(checker.CheckFailure) as caught:
                checker.run([sys.executable, '-c', program, 'secret-sentinel'], timeout=timeout)
            self.assertNotIn('secret-sentinel', str(caught.exception))
            self.assertNotIn(program, str(caught.exception))
        self.assertEqual(checker.run([sys.executable, '-c', "print('ok')"]).stdout.strip(), b'ok')

    def test_compose_always_uses_empty_envfile_and_separate_project(self):
        import tempfile
        checker = self.load()
        with tempfile.TemporaryDirectory(prefix='arknet-ingress-offline-', dir=harness_scratch.scratch_root()) as directory:
            stack = checker.Stack('private-test-core', Path(directory), {'POSTGRES_PASSWORD': 'test-only'}, ['compose.yaml'])
            command = stack.command('config')
            self.assertEqual(command[:2], ['docker', 'compose'])
            self.assertIn('--env-file', command)
            self.assertEqual(Path(command[command.index('--env-file') + 1]).read_bytes(), b'')
            self.assertEqual(command[command.index('--project-name') + 1], 'private-test-core')
            self.assertNotIn('ARKNET_CORE_URL', stack.env)
            self.assertEqual(stack.env['COMPOSE_DISABLE_ENV_FILE'], 'true')

    def test_tls_context_requires_trust_and_hostname_and_certificate_has_san(self):
        import ssl
        import tempfile
        checker = self.load()
        with tempfile.TemporaryDirectory(prefix='arknet-ingress-offline-', dir=harness_scratch.scratch_root()) as directory:
            root = Path(directory)
            checker.make_tls(root)
            context = checker.tls_context(root)
            self.assertTrue(context.check_hostname)
            self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
            result = checker.run([checker.OPENSSL, 'verify', '-CAfile', str(root / 'ca.pem'),
                                  '-verify_hostname', 'localhost', str(root / 'cert.pem')])
            self.assertEqual(result.returncode, 0)
            bad = checker.run([checker.OPENSSL, 'verify', '-CAfile', str(root / 'ca.pem'),
                               '-verify_hostname', 'wrong.example.test', str(root / 'cert.pem')], check=False)
            self.assertNotEqual(bad.returncode, 0)


class ContainerAcceptanceTests(unittest.TestCase):
    @unittest.skipUnless(__import__('os').environ.get('ARKNET_TEST_INGRESS_DOCKER') == '1',
                         'explicit real-container opt-in required')
    def test_real_tls_routes_errors_and_negative_control(self):
        import subprocess
        import sys
        assert SPEC is not None
        result = subprocess.run([sys.executable, str(SPEC.origin)], capture_output=True,
                                text=True, timeout=1500)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('PASS: five real sinks, verified TLS, forwarding, forced errors, negative control', result.stdout)


if __name__ == "__main__":
    unittest.main()
