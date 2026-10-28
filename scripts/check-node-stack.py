#!/usr/bin/env python3
"""Exercise an isolated disposable Node stack; never use an existing project.

Requires Docker Compose and Python 3. No .env file or password is written.
Only the unique test project's containers, networks, and volumes are removed.
"""
import json
from datetime import datetime
import os
from pathlib import Path
import secrets
import socket
import subprocess
import urllib.request
import urllib.error
import re
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy" / "node"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    project = "arknet-node-test-" + uuid.uuid4().hex[:12]
    test_image = project + "-tests"
    env = dict(os.environ,
               POSTGRES_PASSWORD=secrets.token_hex(32),
               POSTGRES_USER="arknet", POSTGRES_DB="arknet_node",
               ARKNET_UI_PORT=str(free_port()), ARKNET_AGENT_PORT=str(free_port()),
               JELLYFIN_BIND="127.0.0.1:" + str(free_port()),
               ARKNET_AGENT_LOG_LEVEL="INFO", PGSSLMODE="disable",
               ARKNET_CORE_URL="", ARKNET_CORE_ALLOW_HTTP="false")
    base = ["docker", "compose", "-p", project]

    def compose(*args, capture=False):
        result = subprocess.run(base + list(args), cwd=DEPLOY, env=env,
                                check=True, text=True, capture_output=capture)
        return result.stdout.strip() if capture else None

    def docker(*args):
        return subprocess.run(["docker", *args], env=env, check=True,
                              capture_output=True, text=True).stdout.strip()

    def sql(statement):
        return compose("exec", "-T", "postgres", "psql", "-U", "arknet",
                       "-d", "arknet_node", "-At", "-c", statement, capture=True)

    expected = {"version": "0.0.1-dev", "state": "PROVISIONING",
                "enrollment_state": "NOT_ENROLLED",
                "core_connectivity": "NOT_IMPLEMENTED",
                "last_successful_core_contact_at": None,
                "database_healthy": True, "agent_healthy": True,
                "jellyfin_healthy": True}
    def status():
        with urllib.request.urlopen(f"http://127.0.0.1:{env['ARKNET_AGENT_PORT']}/api/v1/status", timeout=5) as response:
            return json.load(response)

    try:
        compose("up", "--build", "-d", "--wait", "--wait-timeout", "120")
        ids = {name: compose("ps", "-q", name, capture=True)
               for name in ["postgres", "arknet-agent", "arknet-ui", "jellyfin"]}
        assert all(ids.values()), "All four containers must start"
        for name, container in ids.items():
            info = json.loads(docker("inspect", container))[0]
            assert info["State"]["Health"]["Status"] == "healthy", name
        print("PASS: all four services healthy", flush=True)
        initial = status()
        expected["state_changed_at"] = initial["state_changed_at"]
        datetime.fromisoformat(initial["state_changed_at"].replace("Z", "+00:00"))
        expected["node_id"] = initial["node_id"]
        assert re.fullmatch(r"node_[0-9a-f]{64}", expected["node_id"])
        original_identity = expected["node_id"]
        for port in [env["ARKNET_UI_PORT"], env["ARKNET_AGENT_PORT"]]:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/v1/status", timeout=5) as response:
                assert response.status == 200
                assert response.headers.get_content_type() == "application/json"
                assert json.load(response) == expected
        with urllib.request.urlopen(f"http://127.0.0.1:{env['ARKNET_UI_PORT']}/", timeout=5) as response:
            assert response.status == 200 and b"/src/main.tsx" in response.read()
        print("PASS: UI serves and proxies real Agent status", flush=True)

        docker("build", "--target", "build", "-t", test_image, str(ROOT / "node-agent"))
        env["ARKNET_TEST_DATABASE_URL"] = "postgresql://arknet:" + env["POSTGRES_PASSWORD"] + "@postgres:5432/arknet_node?sslmode=disable"
        subprocess.run(["docker", "run", "--rm", "-e", "GOMAXPROCS=2", "-e", "CGO_ENABLED=0", "--network", project + "_node",
                        "-e", "ARKNET_TEST_DATABASE_URL", test_image,
                        "go", "test", "-trimpath", "-p=1", "-count=1", "-v", "./internal/..."], env=env, check=True)
        print("PASS: real PostgreSQL local state initialization and persistence tests", flush=True)
        def probe(next_state=None, invalid=False):
            env["ARKNET_TEST_NEXT_STATE"] = next_state or ""
            env["ARKNET_TEST_INVALID_TRANSITION"] = "1" if invalid else "0"
            subprocess.run(["docker", "run", "--rm", "-e", "GOMAXPROCS=2", "-e", "CGO_ENABLED=0", "--network", project + "_node",
                "-e", "ARKNET_TEST_DATABASE_URL", "-e", "ARKNET_TEST_NEXT_STATE",
                "-e", "ARKNET_TEST_INVALID_TRANSITION",
                "-v", str(ROOT / "scripts" / "node-state-probe.go") + ":/src/probe/main.go:ro",
                test_image, "go", "run", "-trimpath", "-p=1", "./probe"], env=env, check=True)
        probe(invalid=True)
        assert status() == expected
        assert sql("SELECT count(*) FROM node_state_transitions") == "0"
        probe("ENROLLMENT_PENDING")
        expected["state"] = "ENROLLMENT_PENDING"
        expected["state_changed_at"] = status()["state_changed_at"]
        assert expected["state_changed_at"] != initial["state_changed_at"]
        assert sql("SELECT count(*) FROM node_state_transitions") == "1"
        assert sql("SELECT transitioned_at = (SELECT changed_at FROM node_state) FROM node_state_transitions") == "t"
        assert status() == expected
        compose("restart", "arknet-agent")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert status() == expected
        print("PASS: Agent restart preserves identity and state", flush=True)
        old_agent = compose("ps", "-q", "arknet-agent", capture=True)
        compose("up", "-d", "--no-deps", "--force-recreate", "arknet-agent", "--wait", "--wait-timeout", "120")
        assert compose("ps", "-q", "arknet-agent", capture=True) != old_agent
        assert compose("ps", "-q", "postgres", capture=True) == ids["postgres"]
        assert status() == expected
        ids["arknet-agent"] = compose("ps", "-q", "arknet-agent", capture=True)
        print("PASS: recreating only Agent preserves identity without recreating PostgreSQL", flush=True)

        postgres = json.loads(docker("inspect", ids["postgres"]))[0]
        assert not postgres["HostConfig"]["PortBindings"]
        assert all(value is None for value in postgres["NetworkSettings"]["Ports"].values())
        mounts = postgres["Mounts"]
        assert any(m["Type"] == "volume" and m["Destination"] == "/var/lib/postgresql" for m in mounts)
        print("PASS: PostgreSQL has no host ports and uses a named volume", flush=True)

        # An ephemeral psql client shares the Agent's network namespace.
        # This proves DNS, network access, and authentication without adding
        # a client or database application logic to the Agent image.
        env.update(PGHOST="postgres", PGPORT="5432", PGUSER="arknet",
                   PGDATABASE="arknet_node", PGPASSWORD=env["POSTGRES_PASSWORD"])
        result = docker("run", "--rm", "--network", "container:" + ids["arknet-agent"],
                        "-e", "PGHOST", "-e", "PGPORT", "-e", "PGUSER", "-e", "PGDATABASE",
                        "-e", "PGPASSWORD", "postgres:18-alpine", "psql", "-At", "-c", "SELECT 1")
        assert result == "1"
        print("PASS: authenticated PostgreSQL access from Agent network namespace", flush=True)

        compose("stop", "postgres")
        for path in ["/health", "/api/v1/status"]:
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{env['ARKNET_AGENT_PORT']}{path}", timeout=5)
                raise AssertionError("Agent reported success with database stopped")
            except urllib.error.HTTPError as response:
                assert response.code == 503
                body=json.load(response)
                if path=="/health": assert body=={"healthy":False,"database_healthy":False}
                else: assert body=={"error":"local state database unavailable"}
        print("PASS: database failure produces clear HTTP 503 health/status", flush=True)
        compose("up", "-d", "postgres", "--wait", "--wait-timeout", "120")
        deadline=time.monotonic()+10
        while True:
            try:
                assert status()==expected
                break
            except urllib.error.HTTPError:
                if time.monotonic()>=deadline: raise
                time.sleep(0.2)
        print("PASS: Agent recovers without restart and retains identity", flush=True)

        sql("CREATE TABLE arknet_dev_persistence_probe (value text NOT NULL); "
            "INSERT INTO arknet_dev_persistence_probe VALUES ('retained');")
        compose("restart")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert sql("SELECT value FROM arknet_dev_persistence_probe") == "retained"
        assert status() == expected
        print("PASS: database row survives container restart", flush=True)
        compose("down")
        compose("up", "-d", "--wait", "--wait-timeout", "120")
        assert sql("SELECT value FROM arknet_dev_persistence_probe") == "retained"
        assert status() == expected
        print("PASS: database row survives down/up container recreation", flush=True)
        probe("ERROR")
        probe("PROVISIONING")
        expected["state"] = "PROVISIONING"
        expected["state_changed_at"] = status()["state_changed_at"]
        assert status() == expected
        # Optional local browser check; no browser package is a repository dependency.
        browser_module = os.environ.get("ARKNET_TEST_PLAYWRIGHT_MODULE")
        if browser_module:
            code = """const {chromium} = require(process.env.ARKNET_TEST_PLAYWRIGHT_MODULE);
(async () => { const browser = await chromium.launch({headless:true});
try { const page = await browser.newPage(); const errors=[];
page.on('pageerror', e=>errors.push(e.message));
await page.goto('http://127.0.0.1:'+process.env.ARKNET_UI_PORT);
for (const text of ['0.0.1-dev','PROVISIONING','NOT_IMPLEMENTED','Unknown — not reported by Agent'])
  await page.getByText(text,{exact:true}).waitFor();
if(errors.length) throw new Error(errors.join(';'));
console.log('PASS: browser displays live containerized Agent status');
} finally {await browser.close();}})().catch(e=>{console.error(e);process.exitCode=1;});"""
            subprocess.run(["node", "-e", code], env=env, check=True)
    except Exception:
        subprocess.run(base + ["logs", "--tail", "50"], cwd=DEPLOY, env=env)
        raise
    finally:
        # Destructive cleanup is scoped to the unique disposable test project.
        compose("down", "--volumes", "--remove-orphans")
        if subprocess.run(["docker", "image", "inspect", test_image],capture_output=True).returncode==0:
            docker("image", "rm", test_image)
        print("Removed only disposable test project and helper image: " + project, flush=True)


if __name__ == "__main__":
    main()
