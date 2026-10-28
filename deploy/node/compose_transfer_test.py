import json
import os
import subprocess
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent


class TransferComposeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        env = dict(os.environ)
        env.update({
            "POSTGRES_PASSWORD": "test-only-password",
            "ARKNET_TRANSFER_TOKEN": "t" * 32,
            "ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.arknet.test",
            "ARKNET_TRANSFER_WEB_SEED_ORIGIN": "https://core.arknet.test",
        })
        result = subprocess.run(
            ["docker", "compose", "-f", "compose.yaml", "-f", "compose.transfer.yaml",
             "config", "--format", "json"], cwd=ROOT, env=env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise AssertionError(result.stderr)
        cls.config = json.loads(result.stdout)

    def mounts(self, service):
        return {v["target"]: v for v in self.config["services"][service].get("volumes", [])}

    def test_agent_and_transfer_share_writable_staging_and_media(self):
        agent = self.mounts("arknet-agent")
        transfer = self.mounts("arknet-transfer")
        for target in ("/srv/arknet/transfers", "/srv/arknet/media"):
            self.assertIn(target, agent)
            self.assertFalse(agent[target].get("read_only", False))
            self.assertEqual(agent[target]["source"], transfer[target]["source"])
            self.assertFalse(transfer[target].get("read_only", False))
        self.assertIn("/srv/arknet/transfer-state", transfer)
        self.assertFalse(transfer["/srv/arknet/transfer-state"].get("read_only", False))

    def test_jellyfin_media_is_read_only_and_staging_is_not_exposed(self):
        jellyfin = self.mounts("jellyfin")
        self.assertTrue(jellyfin["/srv/arknet/media"]["read_only"])
        self.assertNotIn("/srv/arknet/transfers", jellyfin)

    def test_transfer_is_unpublished_hardened_and_has_only_control_plus_egress(self):
        service = self.config["services"]["arknet-transfer"]
        self.assertNotIn("ports", service)
        self.assertTrue(service["read_only"])
        self.assertIn("ALL", service["cap_drop"])
        self.assertIn("no-new-privileges:true", service["security_opt"])
        self.assertEqual(set(service["networks"]), {"transfer-control", "transfer-egress"})
        self.assertTrue(self.config["networks"]["transfer-control"]["internal"])


if __name__ == "__main__":
    unittest.main()
