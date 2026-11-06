"""Local checks of live-test scoped credentials; never contacts a VM."""
import importlib.util
import json
from pathlib import Path
import shlex
import unittest
from unittest.mock import patch


PATH = Path(__file__).resolve().parents[1] / "deploy/live-test/ops.py"
spec = importlib.util.spec_from_file_location("live_core_ops", PATH)
assert spec is not None and spec.loader is not None
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)


class LiveCoreAdminTests(unittest.TestCase):
    def test_mints_only_requested_scope_on_core_host(self):
        token = "arknet_adm_" + "A" * 43
        token_id = "admtok_" + "a" * 64
        output = json.dumps({"token": token, "token_id": token_id,
                             "permissions": ["nodes.decide"]}).encode()
        with patch.object(ops, "ssh", return_value=output) as ssh:
            self.assertEqual(ops.mint_core_admin("init-test", "nodes.decide", ttl="1h"), (token, token_id))
        machine, command = ssh.call_args.args
        self.assertEqual(machine["role"], "core")
        args = shlex.split(command.split("arknet-core admin ", 1)[1])
        self.assertEqual(args, ["create-token", "--name", "init-test", "--ttl", "1h",
                                "--permission", "nodes.decide"])
        self.assertNotIn(token, command)

    def test_invalid_scopes_never_contact_a_vm(self):
        for permissions in [(), ("root",), ("swarm",), ("users.admin", "users.admin")]:
            with patch.object(ops, "ssh") as ssh:
                with self.assertRaises(ValueError):
                    ops.mint_core_admin("test-run", *permissions)
                ssh.assert_not_called()

    def test_mismatched_scope_is_not_accepted(self):
        output = json.dumps({"token": "arknet_adm_" + "A" * 43,
                             "permissions": ["users.admin", "nodes.decide"]}).encode()
        with patch.object(ops, "ssh", return_value=output):
            with self.assertRaisesRegex(RuntimeError, "requested scoped token"):
                ops.mint_core_admin("test-run", "users.admin")

    def test_revokes_by_nonsecret_id_on_core_host(self):
        token_id = "admtok_" + "b" * 64
        with patch.object(ops, "ssh", return_value=b"{}") as ssh:
            ops.revoke_core_admin(token_id)
        machine, command = ssh.call_args.args
        self.assertEqual(machine["role"], "core")
        self.assertTrue(command.endswith("revoke-token --token-id " + token_id))


if __name__ == "__main__":
    unittest.main()
