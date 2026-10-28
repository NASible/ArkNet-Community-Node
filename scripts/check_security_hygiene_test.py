import importlib.util
from pathlib import Path


SCRIPT = Path(__file__).with_name("check-security-hygiene.py")
SPEC = importlib.util.spec_from_file_location("security_hygiene", SCRIPT)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def test_clean_examples_are_allowed(tmp_path: Path) -> None:
    (tmp_path / ".env.example").write_text("PASSWORD=\n")
    (tmp_path / "node.env.example").write_text("TOKEN=\n")
    (tmp_path / "README.md").write_text("safe\n")

    assert MODULE.scan(tmp_path) == []


def test_secret_and_runtime_artifacts_are_reported_without_values(tmp_path: Path) -> None:
    artifacts = {
        "deploy/core/.env": "ADMIN_TOKEN=do-not-print\n",
        "deploy/node/node2.env": "PASSWORD=do-not-print\n",
        "build/TRACKER-PRIVATE.P12": "do-not-print\n",
        "dist/id_ed25519": "do-not-print\n",
        "tracker-private.key": "do-not-print\n",
        "state.db-wal": "do-not-print\n",
        "state.sqlite3": "do-not-print\n",
        ".arknet-integration/seed-secret.json": "do-not-print\n",
    }
    for name, value in artifacts.items():
        path = tmp_path / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value)

    violations = MODULE.scan(tmp_path)
    paths = {str(path) for path, _ in violations}
    assert paths == {
        ".arknet-integration",
        "build/TRACKER-PRIVATE.P12",
        "deploy/core/.env",
        "deploy/node/node2.env",
        "dist/id_ed25519",
        "state.db-wal",
        "state.sqlite3",
        "tracker-private.key",
    }
    assert all("do-not-print" not in reason for _, reason in violations)
