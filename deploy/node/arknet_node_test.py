"""Unit tests for deploy/node/arknet-node (no Docker required)."""
import hashlib
import http.server
import importlib.machinery
import importlib.util
import json
import os
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
_loader = importlib.machinery.SourceFileLoader("arknet_node", str(HERE / "arknet-node"))
_spec = importlib.util.spec_from_loader("arknet_node", _loader)
an = importlib.util.module_from_spec(_spec)
_loader.exec_module(an)

SHIPPED = (HERE / "release.json").read_bytes()
COMPOSE = b"services: {}\n"


def manifest(version, channel="development", project="arknet-test", compose=COMPOSE, images=None, extra_files=()):
    m = json.loads(SHIPPED)
    m.update(version=version, channel=channel, published_at="2026-10-25T00:00:00Z")
    for name in an.LOCKSTEP:
        m["components"][name] = {"version": version, "image": f"localhost:5000/arknet/{name}:{version}", "bundled": True}
    for name, image in (images or {}).items():
        m["components"][name]["image"] = image
    m["compose"] = {"project": project, "files": [{"path": "compose.yaml", "sha256": hashlib.sha256(compose).hexdigest()}] +
                    [{"path": n, "sha256": hashlib.sha256(c).hexdigest()} for n, c in extra_files]}
    return json.dumps(m, indent=2).encode()


class Publisher:
    """Loopback release host: /index.json and /releases/<v>/<file>."""

    def __init__(self):
        self.files, self.requests = {}, []
        files, requests = self.files, self.requests

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                body = files.get(self.path)
                self.send_response(200 if body is not None else 404)
                self.end_headers()
                if body is not None:
                    self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def publish(self, version, data, compose=COMPOSE, extra=()):
        self.files[f"/releases/{version}/release.json"] = data
        self.files[f"/releases/{version}/compose.yaml"] = compose
        for name, content in extra:
            self.files[f"/releases/{version}/{name}"] = content
        return hashlib.sha256(data).hexdigest()

    def index(self, version, digest, channel="development"):
        self.files["/index.json"] = json.dumps({"schema": an.INDEX_SCHEMA, "channels": {
            channel: {"version": version, "manifest_url": f"releases/{version}/release.json", "manifest_sha256": digest}}}).encode()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class FakeDocker:
    """Records docker invocations and simulates a running stack."""

    def __init__(self):
        self.calls, self.present, self.pullable = [], set(), set()
        self.running = None  # release version the Agent reports
        self.volumes = {"arknet-test_postgres-data": "2026-10-01T00:00:00Z", "arknet-test_appdata": "2026-10-01T00:00:00Z"}
        self.fail_up_for, self.fail_config, self.fail_dump, self.recreate_volume = set(), False, False, False
        self.jellyfin_healthy = True
        self.stale_agent = False
        self.operator_calls, self.operator_exit = [], 0
        self.node_state = "ENROLLMENT_PENDING"  # Agent keeps reporting the old release (e.g. unreadable manifest)

    def __call__(self, args, stdout=None, stderr=None, text=None, timeout=None):
        assert args[0] == "docker"
        args = args[1:]
        self.calls.append(args)
        ok = lambda out="": subprocess.CompletedProcess(args, 0, out if stdout is subprocess.PIPE else None, "")
        bad = lambda err: subprocess.CompletedProcess(args, 1, "", err)
        if args[:1] == ["info"]:
            return ok("27.0.0")
        if args[:3] == ["compose", "version", "--short"]:
            return ok("2.29.1\n")
        if args[:2] == ["image", "inspect"]:
            return ok() if args[2] in self.present else bad("No such image")
        if args[0] == "pull":
            if args[-1] in self.pullable:
                self.present.add(args[-1])
                return ok()
            return bad("manifest unknown")
        if args[:2] == ["volume", "ls"]:
            return ok("\n".join(self.volumes))
        if args[:2] == ["volume", "inspect"]:
            return ok(self.volumes[args[-1]])
        if args[0] == "compose":
            files = [args[i + 1] for i, a in enumerate(args) if a == "-f"]
            version = Path(files[0]).parent.name
            rest = args[args.index(files[-1]) + 1:]
            if rest[0] == "config":
                return bad("required variable X is missing") if self.fail_config else ok()
            if rest[0] == "up":
                if version in self.fail_up_for:
                    return bad(f"container arknet-agent is unhealthy")
                if self.recreate_volume:
                    self.volumes["arknet-test_postgres-data"] = "2026-10-25T12:00:00Z"
                if not self.stale_agent:
                    self.running = version
                return ok()
            if rest[:4] == ["exec", "-T", "postgres", "sh"]:
                if self.fail_dump:
                    return subprocess.CompletedProcess(args, 1, None, b"connection refused")
                stdout.write(b"PGDMP fake")
                return subprocess.CompletedProcess(args, 0, None, b"")
            if rest[:5] == ["exec", "-T", "arknet-agent", "arknet-agent", "operator"]:
                self.operator_calls.append((rest[5:], stdout))
                return subprocess.CompletedProcess(args, self.operator_exit, None, "")
            if rest[:6] == ["exec", "-T", "arknet-agent", "arknet-agent", "status", "--json"]:
                return ok(json.dumps({"agent_healthy": True, "database_healthy": True, "jellyfin_healthy": self.jellyfin_healthy,
                                      "release": {"version": self.running, "channel": "development"},
                                      "node_id": "node_" + "a" * 64, "state": self.node_state}))
            if rest[:3] == ["exec", "-T", "jellyfin"]:
                return ok("Healthy") if self.jellyfin_healthy else bad("unhealthy")
        raise AssertionError(f"unexpected docker call {args}")

    def ups(self):
        return [Path([c[i + 1] for i, a in enumerate(c) if a == "-f"][0]).parent.name for c in self.calls if "up" in c]


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name)
        self.pub = Publisher()
        self.addCleanup(self.pub.close)
        self.addCleanup(self.tmp.cleanup)
        self.docker = FakeDocker()
        self.install("0.0.2-dev.1")
        self.output = []

    def install(self, version):
        self.home.chmod(0o755)
        d = self.home / "releases" / version
        d.mkdir(parents=True)
        (d / "release.json").write_bytes(manifest(version))
        (d / "compose.yaml").write_bytes(COMPOSE)
        env = self.home / ".env"
        env.write_text(f"POSTGRES_PASSWORD=secret\nARKNET_RELEASE_INDEX_URL={self.pub.base}/index.json\n")
        env.chmod(0o600)
        (self.home / "state.json").write_text(json.dumps({"schema": an.STATE_SCHEMA, "release": version, "previous": None,
                                                          "project": "arknet-test", "compose_files": ["compose.yaml"]}))
        self.docker.running = version
        for name in an.LOCKSTEP:
            self.docker.present.add(f"localhost:5000/arknet/{name}:{version}")
        m = json.loads(manifest(version))
        for name in ("postgresql", "jellyfin"):
            self.docker.present.add(m["components"][name]["image"])

    def publish_newer(self, version="0.0.2-dev.2", data=None, pullable=True):
        data = data or manifest(version)
        digest = self.pub.publish(version, data)
        self.pub.index(version, digest)
        if pullable:
            for name in an.LOCKSTEP:
                self.docker.pullable.add(f"localhost:5000/arknet/{name}:{version}")
        return digest

    def run_cli(self, *argv):
        node = an.Node(self.home, an.Docker(self.docker), out=self.output.append, sleep=lambda s: None)
        # Keep validation fast: a clock that expires immediately after one probe.
        ticks = iter(range(0, 10_000, 1000))
        node.clock = lambda: next(ticks)
        with an.locked(self.home):
            try:
                if argv[0] == "rollback":
                    return node.rollback()
                return node.upgrade(*argv[1:])
            except an.Failure as failure:
                self.output.append(f"FAILED at {failure.step}: {failure.message}")
                return an.EXIT_ABORTED

    def state(self):
        return json.loads((self.home / "state.json").read_text())

    def text(self):
        return "\n".join(self.output)

    def test_upgrade_pulls_backs_up_applies_and_validates(self):
        self.publish_newer()
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_OK, self.text())
        self.assertEqual(self.state()["release"], "0.0.2-dev.2")
        self.assertEqual(self.state()["previous"], "0.0.2-dev.1")
        self.assertEqual(self.docker.ups(), ["0.0.2-dev.2"])
        pulls = [c[-1] for c in self.docker.calls if c[0] == "pull"]
        self.assertEqual(sorted(pulls), sorted(f"localhost:5000/arknet/{n}:0.0.2-dev.2" for n in an.LOCKSTEP))
        last_pull = max(i for i, c in enumerate(self.docker.calls) if c[0] == "pull")
        dump = next(i for i, c in enumerate(self.docker.calls) if "postgres" in c and "exec" in c)
        up_at = next(i for i, c in enumerate(self.docker.calls) if "up" in c)
        self.assertLess(last_pull, dump, "images must be pulled before the backup")
        self.assertLess(dump, up_at, "backup must precede apply")
        up = next(c for c in self.docker.calls if "up" in c)
        for flag in ("--wait", "--no-build", "--remove-orphans"):
            self.assertIn(flag, up)
        self.assertEqual(up[up.index("--pull") + 1], "never")
        self.assertNotIn("down", sum(self.docker.calls, []))
        self.assertNotIn("-v", up)
        backups = list((self.home / "backups").iterdir())
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].stat().st_mode & 0o777, 0o600)
        self.assertIn("UPGRADED: ArkNet 0.0.2-dev.1 -> 0.0.2-dev.2", self.text())
        self.assertNotIn("secret", self.text() + (self.home / "upgrade.log").read_text())
        self.assertTrue((self.home / "releases/0.0.2-dev.1/compose.yaml").exists(), "previous release kept")

    def test_up_to_date_changes_nothing(self):
        self.publish_newer("0.0.2-dev.1")  # same version
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_OK)
        self.assertIn("Nothing to do", self.text())
        self.assertEqual(self.docker.ups(), [])

    def assert_aborted(self, step):
        code = self.run_cli("upgrade")
        self.assertEqual(code, an.EXIT_ABORTED, self.text())
        self.assertIn(f"FAILED at {step}", self.text())
        self.assertEqual(self.docker.ups(), [], "running stack touched")
        self.assertEqual(self.state()["release"], "0.0.2-dev.1")

    def test_digest_mismatch_aborts(self):
        self.publish_newer()
        self.pub.index("0.0.2-dev.2", "0" * 64)
        self.assert_aborted("download")

    def test_tampered_compose_file_aborts(self):
        self.publish_newer()
        self.pub.files["/releases/0.0.2-dev.2/compose.yaml"] = b"services: {evil: {image: x}}\n"
        self.assert_aborted("download")

    def test_unpullable_image_aborts_before_backup(self):
        self.publish_newer(pullable=False)
        self.assert_aborted("images")
        self.assertFalse((self.home / "backups").exists())

    def test_invalid_compose_config_aborts(self):
        self.publish_newer()
        self.docker.fail_config = True
        self.assert_aborted("config")

    def test_backup_failure_aborts(self):
        self.publish_newer()
        self.docker.fail_dump = True
        self.assert_aborted("backup")
        self.assertEqual(list((self.home / "backups").iterdir()), [], "partial dump left behind")

    def test_project_change_channel_change_and_downgrade_rejected(self):
        for data, version in ((manifest("0.0.2-dev.2", project="other"), "0.0.2-dev.2"),
                              (manifest("0.0.1-dev.9"), "0.0.1-dev.9")):
            self.docker.calls.clear()
            self.output.clear()
            digest = self.pub.publish(version, data)
            self.assertEqual(self.run_cli("upgrade", None, f"{self.pub.base}/releases/{version}/release.json", digest), an.EXIT_ABORTED)
            self.assertIn("FAILED at download", self.text())
        self.output.clear()
        digest = self.pub.publish("0.1.0", manifest("0.1.0", channel="stable"))
        self.assertEqual(self.run_cli("upgrade", None, f"{self.pub.base}/releases/0.1.0/release.json", digest), an.EXIT_ABORTED)
        self.assertIn("stable channel", self.text())

    def test_modified_installed_release_is_refused(self):
        (self.home / "releases/0.0.2-dev.1/compose.yaml").write_text("services: {edited: {}}\n")
        self.publish_newer()
        self.assert_aborted("prerequisites")
        self.assertIn("modified", self.text())

    def test_unreadable_release_for_agent_is_refused(self):
        self.home.chmod(0o700)
        self.publish_newer()
        self.assert_aborted("prerequisites")
        self.assertIn("not accessible to other users", self.text())
        self.output.clear()
        an.main(["--home", str(self.home), "upgrade"], docker=an.Docker(self.docker), out=self.output.append)
        self.assertIn(f"chmod 755 {self.home}", self.text())

    def test_downloaded_release_is_readable_by_agent_despite_umask(self):
        self.publish_newer()
        old = os.umask(0o077)
        try:
            self.assertEqual(self.run_cli("upgrade"), an.EXIT_OK, self.text())
        finally:
            os.umask(old)
        d = self.home / "releases" / "0.0.2-dev.2"
        self.assertEqual(d.stat().st_mode & 0o777, 0o755)
        self.assertEqual((d / "release.json").stat().st_mode & 0o777, 0o644)

    def test_failure_message_is_not_repeated(self):
        self.publish_newer(pullable=False)
        node = an.main(["--home", str(self.home), "upgrade"], docker=an.Docker(self.docker), out=self.output.append)
        self.assertEqual(node, an.EXIT_ABORTED)
        self.assertEqual(self.text().count("not changed"), 1, self.text())

    def test_world_readable_env_is_refused(self):
        (self.home / ".env").chmod(0o644)
        self.publish_newer()
        self.assert_aborted("prerequisites")

    def test_failed_apply_rolls_back_and_reports(self):
        self.publish_newer()
        self.docker.fail_up_for.add("0.0.2-dev.2")
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_ROLLED_BACK, self.text())
        self.assertEqual(self.docker.ups(), ["0.0.2-dev.2", "0.0.2-dev.1"])
        self.assertEqual(self.state()["release"], "0.0.2-dev.1")
        self.assertIn("UPGRADE FAILED while applying ArkNet 0.0.2-dev.2: container arknet-agent is unhealthy", self.text())
        self.assertIn("ROLLED BACK", self.text())
        self.assertIn("backup", self.text().lower())

    def test_unhealthy_jellyfin_after_apply_rolls_back(self):
        self.publish_newer()
        original = self.docker.__call__

        def flaky(args, **kw):
            self.docker.jellyfin_healthy = self.docker.running != "0.0.2-dev.2"
            return original(args, **kw)
        node_docker = an.Docker(flaky)
        node = an.Node(self.home, node_docker, out=self.output.append, sleep=lambda s: None)
        ticks = iter(range(0, 10_000, 1000))
        node.clock = lambda: next(ticks)
        self.assertEqual(node.upgrade(), an.EXIT_ROLLED_BACK, self.text())
        self.assertIn("Jellyfin unhealthy", self.text())

    def test_agent_not_running_target_release_rolls_back(self):
        self.publish_newer()
        self.docker.stale_agent = True
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_ROLLED_BACK, self.text())
        self.assertIn("ArkNet Agent is not running release 0.0.2-dev.2", self.text())
        self.assertEqual(self.state()["release"], "0.0.2-dev.1")

    def test_same_version_is_not_reinstalled(self):
        data = manifest("0.0.2-dev.1")
        digest = self.pub.publish("0.0.2-dev.1", data)
        code = self.run_cli("upgrade", None, f"{self.pub.base}/releases/0.0.2-dev.1/release.json", digest)
        self.assertEqual(code, an.EXIT_ABORTED)
        self.assertIn("is not newer", self.text())
        self.assertEqual(self.docker.ups(), [])

    def test_unreadable_manifest_file_is_refused(self):
        (self.home / "releases/0.0.2-dev.1/release.json").chmod(0o600)
        self.publish_newer()
        self.assert_aborted("prerequisites")
        self.assertIn("release.json not accessible", self.text())

    def test_recreated_volume_is_detected(self):
        self.publish_newer()
        self.docker.recreate_volume = True
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_ATTENTION, self.text())
        self.assertIn("persistent volume(s) missing or recreated", self.text())

    def test_failed_rollback_needs_attention(self):
        self.publish_newer()
        self.docker.fail_up_for.update({"0.0.2-dev.2", "0.0.2-dev.1"})
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_ATTENTION)
        self.assertIn("ATTENTION", self.text())
        self.assertIn("Volumes were not removed", self.text())

    def test_rollback_command_returns_to_previous(self):
        self.publish_newer()
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_OK)
        self.docker.calls.clear()
        self.assertEqual(self.run_cli("rollback"), an.EXIT_OK, self.text())
        self.assertEqual(self.docker.ups(), ["0.0.2-dev.1"])
        self.assertEqual(self.state()["release"], "0.0.2-dev.1")
        self.assertEqual(self.state()["previous"], "0.0.2-dev.2")

    def test_operator_commands_run_agent_cli_with_uncaptured_output(self):
        for argv, expected in ((["operator", "setup-code"], ["setup-code"]),
                               (["operator", "setup-code", "--additional"], ["setup-code", "--additional"]),
                               (["operator", "recover", "alice"], ["recover", "alice"]),
                               (["operator", "disable", "alice"], ["disable", "alice"]),
                               (["operator", "list"], ["list"])):
            self.docker.operator_calls.clear()
            code = an.main(["--home", str(self.home), *argv], docker=an.Docker(self.docker), out=self.output.append)
            self.assertEqual(code, an.EXIT_OK, self.text())
            self.assertEqual(len(self.docker.operator_calls), 1)
            args, stdout = self.docker.operator_calls[0]
            self.assertEqual(args, expected)
            # Codes are printed by the container straight to the terminal, never captured.
            self.assertIsNone(stdout)
        self.docker.operator_exit = 1
        self.assertEqual(an.main(["--home", str(self.home), "operator", "setup-code"], docker=an.Docker(self.docker),
                                 out=self.output.append), 1)

    def test_health_check_uses_local_status_command(self):
        self.publish_newer()
        self.assertEqual(self.run_cli("upgrade"), an.EXIT_OK, self.text())
        execs = [c for c in self.docker.calls if "exec" in c and "arknet-agent" in c]
        self.assertTrue(execs)
        self.assertFalse([c for c in execs if "wget" in c or any("/api/v1/status" in a for a in c)])

    def test_concurrent_operations_are_refused(self):
        with an.locked(self.home):
            with self.assertRaises(an.Failure):
                with an.locked(self.home):
                    pass


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name) / "arknet"
        self.home.mkdir(mode=0o755)
        env = self.home / ".env"
        env.write_text("POSTGRES_PASSWORD=secret\nARKNET_CORE_URL=https://core.example\n")
        env.chmod(0o600)
        self.pub = Publisher()
        self.addCleanup(self.pub.close)
        self.docker = FakeDocker()
        self.docker.volumes = {}
        self.output = []
        data = manifest("0.1.0", channel="stable")
        self.pub.index("0.1.0", self.pub.publish("0.1.0", data), channel="stable")
        m = json.loads(data)
        for name in an.LOCKSTEP:
            self.docker.pullable.add(m["components"][name]["image"])
        for name in ("postgresql", "jellyfin"):
            self.docker.present.add(m["components"][name]["image"])

    def install(self, channel="stable"):
        node = an.Node(self.home, an.Docker(self.docker), out=self.output.append, sleep=lambda s: None)
        ticks = iter(range(0, 100_000, 1000))
        node.clock = lambda: next(ticks)
        try:
            return node.install(f"{self.pub.base}/index.json", channel)
        except an.Failure as failure:
            self.output.append(f"FAILED at {failure.step}: {failure.message}")
            return an.EXIT_ABORTED

    def text(self):
        return "\n".join(self.output)

    def test_fresh_install_verifies_pulls_starts_and_reports_enrollment(self):
        self.assertEqual(self.install(), an.EXIT_OK, self.text())
        state = json.loads((self.home / "state.json").read_text())
        self.assertEqual((state["release"], state["previous"], state["compose_files"]), ("0.1.0", None, ["compose.yaml"]))
        self.assertEqual(self.docker.ups(), ["0.1.0"])
        self.assertEqual(len([c for c in self.docker.calls if c[0] == "pull"]), 3)
        self.assertIn("INSTALLED: ArkNet 0.1.0 (stable)", self.text())
        self.assertIn("Node state: ENROLLMENT_PENDING (enrollment requested; an ArkNet Core administrator must approve this Node)", self.text())
        self.assertIn("ssh -L 5174:127.0.0.1:5174", self.text())
        self.assertEqual((self.home / "releases").stat().st_mode & 0o777, 0o755)
        self.assertNotIn("secret", self.text())

    def test_rerun_restarts_installed_release_without_downloading(self):
        self.assertEqual(self.install(), an.EXIT_OK)
        before_env = (self.home / ".env").read_bytes()
        self.pub.requests.clear()
        self.docker.calls.clear()
        self.output.clear()
        self.assertEqual(self.install(), an.EXIT_OK, self.text())
        self.assertIn("already installed", self.text())
        self.assertEqual(self.pub.requests, [], "re-run must not download")
        self.assertEqual(self.docker.ups(), ["0.1.0"])
        self.assertEqual((self.home / ".env").read_bytes(), before_env)

    def test_channel_without_release_is_reported(self):
        self.assertEqual(self.install("development"), an.EXIT_ABORTED)
        self.assertIn("No development ArkNet release is published yet", self.text())
        self.assertFalse((self.home / "state.json").exists())

    def test_core_not_answering_warns_but_installs(self):
        self.docker.node_state = "PROVISIONING"
        self.assertEqual(self.install(), an.EXIT_OK)
        self.assertIn("WARNING: ArkNet Core at https://core.example has not accepted", self.text())

    def test_failed_start_is_reported_and_rerun_is_safe(self):
        self.docker.fail_up_for.add("0.1.0")
        self.assertEqual(self.install(), an.EXIT_ATTENTION)
        self.assertIn("INSTALL FAILED while starting the Node", self.text())
        self.assertIn("logs --tail 50", self.text())
        self.assertFalse((self.home / "state.json").exists(), "a failed install must not be recorded")
        self.docker.fail_up_for.clear()
        self.output.clear()
        self.assertEqual(self.install(), an.EXIT_OK, self.text())


class ValidationTests(unittest.TestCase):
    def test_shipped_manifest_is_valid(self):
        self.assertEqual(an.validate_manifest(SHIPPED)["components"]["arknet-agent"]["version"], json.loads(SHIPPED)["version"])

    def test_manifest_rules_match_agent_parser(self):
        base = json.loads(manifest("0.1.0-dev.1"))
        cases = {
            "unknown field": lambda m: m.update(extra=1),
            "stable channel with pre-release": lambda m: m.update(channel="stable"),
            "latest tag": lambda m: m["components"]["jellyfin"].update(image="jellyfin/jellyfin:latest"),
            "untagged with port": lambda m: m["components"]["arknet-ui"].update(image="localhost:5000/arknet-ui"),
            "digest only": lambda m: m["components"]["jellyfin"].update(image="jellyfin/jellyfin@sha256:" + "a" * 64),
            "not lockstep": lambda m: m["components"]["arknet-agent"].update(version="0.0.9"),
            "missing component": lambda m: m["components"].pop("tailscale"),
            "path escape": lambda m: m["compose"]["files"][0].update(path="../compose.yaml"),
            "jellyfin unbundled": lambda m: m["components"]["jellyfin"].update(bundled=False),
        }
        for name, mutate in cases.items():
            m = json.loads(json.dumps(base))
            mutate(m)
            with self.subTest(name), self.assertRaises(ValueError):
                an.validate_manifest(json.dumps(m).encode())
        with self.assertRaises(ValueError):
            an.validate_manifest(b'{"schema":"a","schema":"b"}')

    def test_semver_precedence(self):
        ordered = ["0.0.1-dev", "0.0.1-dev.1", "0.0.1-dev.2", "0.0.1-dev.10", "0.0.1-dev.x", "0.0.1-rc.1", "0.0.1",
                   "0.0.2-dev", "0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta.11", "0.1.0", "0.10.0", "1.0.0"]
        for i, a in enumerate(ordered):
            for j, b in enumerate(ordered):
                self.assertEqual(an.compare_versions(a, b), (i > j) - (i < j), (a, b))

    def test_url_policy(self):
        for url, ok in (("https://releases.example/i.json", True), ("http://127.0.0.1:1/i.json", True),
                        ("http://releases.example/i.json", False), ("https://u:p@x/i", False), ("file:///etc/passwd", False)):
            with self.subTest(url):
                if ok:
                    an.check_url(url, False)
                else:
                    self.assertRaises(ValueError, an.check_url, url, False)


if __name__ == "__main__":
    unittest.main()
