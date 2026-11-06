import json
import unittest

from scripts import core_admin


class MintAdminTokenTests(unittest.TestCase):
    def test_runs_the_shell_cli_inside_the_core_container_with_only_the_named_permissions(self):
        calls = []
        token = core_admin.TOKEN_PREFIX + "A" * 43

        def compose(*args, capture=False, **_):
            calls.append((args, capture))
            return json.dumps({"token": token, "token_id": "admtok_" + "1" * 64, "permissions": ["nodes.decide"]})

        self.assertEqual(core_admin.mint_admin_token(compose, "e2e-run", "nodes.decide"), token)
        args, capture = calls[0]
        self.assertTrue(capture)
        self.assertEqual(args[:5], ("exec", "-T", "arknet-core", "arknet-core", "admin"))
        self.assertEqual(list(args[5:]), ["create-token", "--name", "e2e-run", "--permission", "nodes.decide", "--ttl", "2h"])

    def test_refuses_unscoped_or_unknown_requests_and_bad_output(self):
        compose = lambda *a, **k: json.dumps({"token": "nope"})
        with self.assertRaises(ValueError):
            core_admin.mint_admin_token(compose, "x-run")  # no permission
        with self.assertRaises(ValueError):
            core_admin.mint_admin_token(compose, "x-run", "root")
        with self.assertRaises(AssertionError):
            core_admin.mint_admin_token(compose, "x-run", "users.admin")

    def test_permission_mismatch_in_output_is_refused(self):
        token = core_admin.TOKEN_PREFIX + "A" * 43
        compose = lambda *a, **k: json.dumps({"token": token, "permissions": ["nodes.decide", "users.admin"]})
        with self.assertRaises(AssertionError):
            core_admin.mint_admin_token(compose, "x-run", "nodes.decide")


class ScopedAdminTests(unittest.TestCase):
    def test_mints_once_on_first_use_and_tracks_the_secret_for_leak_checks(self):
        calls = []
        token = core_admin.TOKEN_PREFIX + "B" * 43

        def compose(*args, **_):
            calls.append(args)
            return json.dumps({"token": token, "permissions": ["media.admin", "nodes.decide"]})

        admin = core_admin.ScopedAdmin(compose, "run-x", "nodes.decide", "media.admin")
        self.assertEqual(calls, [])
        self.assertEqual(admin.secrets(), [])
        self.assertEqual(admin.headers(), {"Authorization": "Bearer " + token})
        self.assertEqual(admin.token, token)
        self.assertEqual(len(calls), 1)
        self.assertEqual(admin.secrets(), [token])


if __name__ == "__main__":
    unittest.main()
