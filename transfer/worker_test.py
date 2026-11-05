import importlib.util
import json
import os
import tempfile
import threading
import time
import unittest
from pathlib import Path
from types import SimpleNamespace

import libtorrent as lt

SPEC = importlib.util.spec_from_file_location("arknet_worker", Path(__file__).with_name("worker.py"))
worker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(worker)

HASH = "b" * 64
OBJECT_ID = "media_" + "a" * 64
ORIGIN = "https://core.arknet.test"
WEB_SEED = ORIGIN + "/api/web-seed/v1/media/" + OBJECT_ID + "/YWJj.ZGVm"
TORRENT_URL = ORIGIN + "/api/torrent/v1/media/" + OBJECT_ID + "/YWJj.ZGVm"
TRACKER_ORIGIN = "https://tracker.arknet.test"
TRACKER = TRACKER_ORIGIN + "/announce/YWJj.ZGVm"


class Hashes:
    def __init__(self, v1=False, v2=True):
        self._v1, self._v2 = v1, v2
        self.v2 = HASH

    def has_v1(self): return self._v1
    def has_v2(self): return self._v2


class Info:
    def __init__(self, private=True, v1=False, v2=True, size=4096, path=OBJECT_ID):
        self.values = (private, v1, v2, size, path)

    def priv(self): return self.values[0]
    def info_hashes(self): return Hashes(self.values[1], self.values[2])
    def files(self): return self
    def num_files(self): return 1
    def file_flags(self, _): return 0
    def file_path(self, _): return self.values[4]
    def file_size(self, _): return self.values[3]


class FakeHandle:
    def __init__(self, urls=None, http=None):
        self.urls = list(urls or [])
        self.http = list(http or [])
        self.trackers = None
        self.resume = {}

    def status(self): return SimpleNamespace(has_metadata=True)
    def torrent_file(self): return Info()
    def replace_trackers(self, trackers): self.trackers = trackers
    def url_seeds(self): return list(self.urls)
    def http_seeds(self): return list(self.http)
    def remove_url_seed(self, value): self.urls.remove(value)
    def remove_http_seed(self, value): self.http.remove(value)
    def add_url_seed(self, value): self.urls.append(value)
    def write_resume_data(self): return dict(self.resume)


class RecordingManager:
    def __init__(self):
        self.added = None

    def add(self, object_id, size, info_hash, tracker_url="", web_seed_url="", torrent_url=""):
        self.added = (object_id, size, info_hash, tracker_url, web_seed_url, torrent_url)
        return {"object_id": object_id}


class IdentityConnector:
    """Source-filter unit tests don't make network calls; record original URL."""
    def authorize(self, *args, **kwargs):
        pass
    def tracker_url(self, info_hash, original):
        return original
    def remove(self, info_hash):
        pass
    def close(self):
        pass


def fake_manager():
    value = worker.Manager.__new__(worker.Manager)
    value.connector = IdentityConnector()
    return value


class WorkerPolicyTests(unittest.TestCase):
    def test_configured_rate_limit_also_applies_to_private_network_peers(self):
        session = lt.session(worker.session_settings("127.0.0.1:0", 512 * 1024))
        self.assertFalse(session.get_settings()["ignore_limits_on_local_network"])
        self.assertEqual(session.get_settings()["download_rate_limit"], 512 * 1024)

    def test_zero_libtorrent_error_code_is_not_failure(self):
        self.assertFalse(worker.has_torrent_error(SimpleNamespace(errc=lt.error_code())))

    def test_session_and_torrent_policy_disable_public_discovery(self):
        settings = worker.session_settings("0.0.0.0:0", 0)
        self.assertFalse(settings["enable_dht"])
        self.assertFalse(settings["enable_lsd"])
        self.assertFalse(settings["enable_upnp"])
        self.assertFalse(settings["enable_natpmp"])
        self.assertEqual(settings["max_pex_peers"], 0)
        self.assertTrue(settings["ssrf_mitigation"])
        self.assertFalse(worker.session_settings("0.0.0.0:0", 0, True)["ssrf_mitigation"])
        for flag in (lt.torrent_flags.disable_dht, lt.torrent_flags.disable_lsd,
                     lt.torrent_flags.disable_pex, lt.torrent_flags.override_trackers,
                     lt.torrent_flags.override_web_seeds):
            self.assertEqual(worker.SECURE_FLAGS & flag, flag)

    def test_private_v2_metadata_uses_exact_authorized_sources(self):
        with tempfile.TemporaryDirectory() as root:
            staging = os.path.join(root, "staging")
            payload = Path(root) / OBJECT_ID
            payload.write_bytes(b"x" * 4096)
            files = lt.file_storage()
            files.add_file(OBJECT_ID, 4096)
            creator = lt.create_torrent(files, 16 * 1024, lt.create_torrent.v2_only)
            creator.set_priv(True)
            lt.set_piece_hashes(creator, root)
            metadata = lt.bencode(creator.generate())
            info = lt.torrent_info(metadata)
            info_hash = str(info.info_hashes().v2).lower()
            for supplied, expected in (("", []), (WEB_SEED, [WEB_SEED])):
                params = worker.new_params(OBJECT_ID, 4096, info_hash,
                                           "https://tracker.arknet.test/announce/private",
                                           staging, supplied, metadata)
                hashes = params.ti.info_hashes()
                self.assertTrue(hashes.has_v2())
                self.assertFalse(hashes.has_v1())
                self.assertEqual(str(hashes.v2), info_hash)
                self.assertIsNotNone(params.ti)
                self.assertEqual(params.trackers, ["https://tracker.arknet.test/announce/private"])
                self.assertEqual(params.url_seeds, expected)
                self.assertEqual(params.http_seeds, [])
                self.assertFalse(params.flags & lt.torrent_flags.paused)
                self.assertEqual(params.save_path, os.path.join(staging, OBJECT_ID))

    def test_metadata_requires_private_v2_exact_hash_and_size(self):
        self.assertTrue(worker.valid_metadata(Info(), HASH, 4096))
        for info in (Info(private=False), Info(v1=True), Info(v2=False), Info(size=4095)):
            self.assertFalse(worker.valid_metadata(info, HASH, 4096))

    def test_worker_revalidates_exact_web_seed_route_and_origin(self):
        self.assertTrue(worker.valid_web_seed_url(WEB_SEED, ORIGIN, OBJECT_ID))
        bad = [
            "https://evil.test/api/web-seed/v1/media/" + OBJECT_ID + "/YWJj.ZGVm",
            ORIGIN + "/other/" + OBJECT_ID + "/YWJj.ZGVm",
            WEB_SEED + "?secret=x", WEB_SEED + "#secret",
            "https://user:secret@core.arknet.test/api/web-seed/v1/media/" + OBJECT_ID + "/YWJj.ZGVm",
            WEB_SEED.replace("/api/", "/%61pi/"), WEB_SEED.replace("YWJj.ZGVm", "YWJj=.ZGVm"),
            WEB_SEED.replace(OBJECT_ID, "media_" + "c" * 64), WEB_SEED + "/filename",
        ]
        for value in bad:
            with self.subTest(value=value):
                self.assertFalse(worker.valid_web_seed_url(value, ORIGIN, OBJECT_ID))

    def test_worker_revalidates_exact_torrent_metadata_route_and_origin(self):
        self.assertTrue(worker.valid_torrent_url(TORRENT_URL, ORIGIN, OBJECT_ID))
        for value in (WEB_SEED, TORRENT_URL + "?secret=x", TORRENT_URL + "/extra",
                      TORRENT_URL.replace("https://core.arknet.test", "https://evil.test")):
            with self.subTest(value=value):
                self.assertFalse(worker.valid_torrent_url(value, ORIGIN, OBJECT_ID))

    def test_worker_accepts_only_canonical_object_scoped_tracker_grant_route(self):
        self.assertTrue(worker.valid_tracker_url(TRACKER, TRACKER_ORIGIN))
        bad = [
            "https://evil.test/announce/YWJj.ZGVm", TRACKER_ORIGIN + "/announce",
            TRACKER_ORIGIN + "/announce/", TRACKER_ORIGIN + "/announce/public",
            TRACKER_ORIGIN + "/announce/YWJj.ZGVm/extra", TRACKER + "?secret=x",
        ]
        for value in bad:
            with self.subTest(value=value):
                self.assertFalse(worker.valid_tracker_url(value, TRACKER_ORIGIN))

    def test_exact_add_request_requires_web_seed_string_and_validates_it(self):
        request = {"id": 1, "op": "add", "object_id": OBJECT_ID, "size": 4096,
                   "info_hash_v2": HASH, "tracker_url": TRACKER, "web_seed_url": WEB_SEED,
                   "torrent_url": TORRENT_URL}
        self.assertEqual(worker.exact_request(request, TRACKER_ORIGIN, ORIGIN), "add")
        request["web_seed_url"] = ""
        with self.assertRaisesRegex(worker.WorkerError, "^invalid_request$"):
            worker.exact_request(request, TRACKER_ORIGIN, ORIGIN)
        for value in (None, 1, WEB_SEED + "?x=1"):
            request["web_seed_url"] = value
            with self.assertRaisesRegex(worker.WorkerError, "^invalid_request$"):
                worker.exact_request(request, TRACKER_ORIGIN, ORIGIN)
        del request["web_seed_url"]
        with self.assertRaisesRegex(worker.WorkerError, "^invalid_request$"):
            worker.exact_request(request, TRACKER_ORIGIN, ORIGIN)

    def test_dispatch_forwards_authorized_web_seed_to_manager(self):
        manager = RecordingManager()
        request = {"id": 7, "op": "add", "object_id": OBJECT_ID, "size": 4096,
                   "info_hash_v2": HASH, "tracker_url": TRACKER, "web_seed_url": WEB_SEED,
                   "torrent_url": TORRENT_URL}
        result = worker.dispatch_request(manager, request, TRACKER_ORIGIN, ORIGIN)
        self.assertEqual(result, {"id": 7, "status": {"object_id": OBJECT_ID}})
        self.assertEqual(manager.added, (OBJECT_ID, 4096, HASH, TRACKER, WEB_SEED, TORRENT_URL))

    def test_metadata_filter_keeps_only_authorized_url_and_removes_http_seeds(self):
        manager = fake_manager()
        handle = FakeHandle([WEB_SEED, "https://evil.test/seed", WEB_SEED], ["https://evil.test/http"])
        entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                              "state": "DOWNLOADING"}, handle, TRACKER, WEB_SEED)
        self.assertTrue(manager._secure_metadata(entry))
        self.assertEqual(handle.urls, [WEB_SEED])
        self.assertEqual(handle.http, [])
        self.assertEqual([entry.url for entry in handle.trackers], [TRACKER])

    def test_periodic_refresh_secures_sources_once_per_handle_without_web_seed_churn(self):
        # Re-adding the URL seed on every refresh drops libtorrent's in-flight web-seed
        # connection, wasting Core egress and starving constrained transfers.
        class CountingHandle(FakeHandle):
            removed = added = 0
            def remove_url_seed(self, value): self.removed += 1; super().remove_url_seed(value)
            def add_url_seed(self, value): self.added += 1; super().add_url_seed(value)
        manager = fake_manager()
        first = CountingHandle([WEB_SEED, "https://evil.test/seed"])
        entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                              "state": "DOWNLOADING", "metadata_validated": True}, first, TRACKER, WEB_SEED)
        for _ in range(5):
            self.assertTrue(manager._secure_metadata(entry))
        self.assertEqual((first.removed, first.added, first.urls), (2, 1, [WEB_SEED]))
        replacement = CountingHandle(["https://evil.test/seed"])
        entry.handle = replacement
        self.assertTrue(manager._secure_metadata(entry))
        self.assertTrue(manager._secure_metadata(entry))
        self.assertEqual((replacement.removed, replacement.added, replacement.urls), (1, 1, [WEB_SEED]))

    def test_completed_transfer_leaves_swarm_and_does_not_seed(self):
        # Receiving must not imply serving: Peer Serving is a separate opt-in.
        class SeedingHandle(FakeHandle):
            def status(self):
                return SimpleNamespace(has_metadata=True, errc=lt.error_code(), total_done=4096,
                                       total_wanted_done=4096, is_seeding=True)
        class Session:
            removed = []
            def remove_torrent(self, handle): self.removed.append(handle)
        with tempfile.TemporaryDirectory() as root:
            manager = fake_manager()
            manager.session, manager.state_dir = Session(), Path(root)
            handle = SeedingHandle([WEB_SEED])
            entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                                  "state": "DOWNLOADING", "metadata_validated": True}, handle, TRACKER, WEB_SEED)
            manager._refresh(entry, persist=True)
            self.assertEqual(entry.state, "COMPLETED")
            self.assertEqual(entry.completed_bytes, 4096)
            self.assertEqual(manager.session.removed, [handle])
            self.assertIsNone(entry.handle)
            self.assertEqual(json.loads(manager.manifest_path(OBJECT_ID).read_text())["state"], "COMPLETED")
            manager._refresh(entry, persist=True)
            self.assertEqual(manager.session.removed, [handle])

    def test_entry_manifest_omits_web_seed_and_restored_entry_starts_without_one(self):
        entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                              "state": "DOWNLOADING"}, tracker_url=TRACKER, web_seed_url=WEB_SEED)
        self.assertEqual(entry.tracker_url, TRACKER)
        self.assertEqual(entry.web_seed_url, WEB_SEED)
        self.assertNotIn("web_seed_url", entry.manifest())
        self.assertNotIn("tracker_url", entry.manifest())
        restored = worker.Entry(entry.manifest())
        self.assertEqual(restored.web_seed_url, "")
        self.assertEqual(restored.tracker_url, "")

    def test_existing_active_add_refreshes_runtime_web_seed(self):
        manager = fake_manager()
        manager.lock = threading.RLock()
        handle = FakeHandle(["https://evil.test/old"], ["https://evil.test/http"])
        entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                              "state": "DOWNLOADING"}, handle)
        manager.entries = {OBJECT_ID: entry}
        manager.web_seed_origin = ORIGIN
        manager.tracker_origin = TRACKER_ORIGIN
        manager.status = lambda ignored: {"object_id": OBJECT_ID}
        result = manager.add(OBJECT_ID, 4096, HASH, TRACKER, WEB_SEED, TORRENT_URL)
        self.assertEqual(result["object_id"], OBJECT_ID)
        self.assertEqual(entry.web_seed_url, WEB_SEED)
        self.assertEqual(handle.urls, [WEB_SEED])
        self.assertEqual(handle.http, [])
        self.assertEqual([entry.url for entry in handle.trackers], [TRACKER])

    def test_restore_forces_nonterminal_transfer_paused_without_authorization(self):
        with tempfile.TemporaryDirectory() as root:
            state = Path(root)
            (state / (OBJECT_ID + ".json")).write_text(json.dumps({
                "object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                "state": "DOWNLOADING", "error_code": "", "completed_bytes": 128,
            }))
            manager = fake_manager()
            manager.state_dir = state
            manager.entries = {}
            manager._restore()
            restored = manager.entries[OBJECT_ID]
            self.assertEqual(restored.state, "PAUSED")
            self.assertIsNone(restored.handle)
            self.assertEqual(restored.tracker_url, "")
            self.assertEqual(restored.web_seed_url, "")

    def test_resume_write_scrubs_all_url_seed_key_variants_before_bencoding(self):
        with tempfile.TemporaryDirectory() as root:
            manager = fake_manager()
            manager.state_dir = Path(root)
            handle = FakeHandle()
            handle.resume = {
                "file-format": "libtorrent resume file",
                "url-list": WEB_SEED, b"url-list": WEB_SEED.encode(),
                "httpseeds": [WEB_SEED], b"httpseeds": [WEB_SEED.encode()],
                "url_seeds": [WEB_SEED], b"url_seeds": [WEB_SEED.encode()],
                "http_seeds": [WEB_SEED], b"http_seeds": [WEB_SEED.encode()],
                "trackers": [[TRACKER]], b"trackers": [[TRACKER.encode()]],
            }
            entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                                  "state": "DOWNLOADING"}, handle, TRACKER, WEB_SEED)
            captured = {}
            original = worker.atomic_write
            worker.atomic_write = lambda path, data: captured.setdefault("data", data)
            try:
                manager._save_resume(entry, force=True)
            finally:
                worker.atomic_write = original
            decoded = lt.bdecode(captured["data"])
            keys = {key.decode() if isinstance(key, bytes) else key for key in decoded}
            self.assertTrue(keys.isdisjoint({"url-list", "httpseeds", "url_seeds", "http_seeds", "trackers"}))
            self.assertNotIn(WEB_SEED.encode(), captured["data"])
            self.assertNotIn(TRACKER.encode(), captured["data"])


    def test_refresh_keeps_live_web_seed_until_rotation_age(self):
        # A fresh grant must not drop the in-flight web-seed range: libtorrent
        # requests the remaining file as one range and discards it on removal.
        class CountingHandle(FakeHandle):
            removed = added = 0
            def remove_url_seed(self, value): self.removed += 1; super().remove_url_seed(value)
            def add_url_seed(self, value): self.added += 1; super().add_url_seed(value)
        manager = fake_manager()
        handle = CountingHandle()
        entry = worker.Entry({"object_id": OBJECT_ID, "size": 4096, "info_hash_v2": HASH,
                              "state": "DOWNLOADING"}, handle, TRACKER, WEB_SEED)
        manager._apply_authorization(entry, now=1000.0)
        self.assertEqual((handle.removed, handle.added, handle.urls), (0, 1, [WEB_SEED]))
        fresh = WEB_SEED.replace("YWJj.ZGVm", "Z2hp.amts")
        entry.web_seed_url = fresh
        manager._apply_authorization(entry, now=1000.0 + worker.WEB_SEED_ROTATE_SECONDS - 1)
        self.assertEqual((handle.removed, handle.added, handle.urls), (0, 1, [WEB_SEED]))
        manager._apply_authorization(entry, now=1000.0 + worker.WEB_SEED_ROTATE_SECONDS)
        self.assertEqual((handle.removed, handle.added, handle.urls), (1, 2, [fresh]))
        # An unexpected URL seed on the handle is never kept, regardless of age.
        handle.urls.append("https://evil.test/seed")
        manager._apply_authorization(entry, now=1000.0 + worker.WEB_SEED_ROTATE_SECONDS + 1)
        self.assertEqual(handle.urls, [fresh])
        # A replacement handle always receives the current authorization.
        replacement = CountingHandle()
        entry.handle = replacement
        manager._apply_authorization(entry, now=1000.0 + worker.WEB_SEED_ROTATE_SECONDS + 2)
        self.assertEqual(replacement.urls, [fresh])
        self.assertTrue(worker.WEB_SEED_ROTATE_SECONDS * 2 < 5 * 60, "must rotate well inside Core's grant lifetime")


class RangeSeed:
    """Local HTTP Range server standing in for Core's web seed and metadata routes."""

    def __init__(self, root, size=2 * 1024 * 1024, piece=64 * 1024):
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
        self.data = os.urandom(size)
        self.size = size
        source = Path(root) / "source"
        source.mkdir()
        (source / OBJECT_ID).write_bytes(self.data)
        files = lt.file_storage()
        lt.add_files(files, str(source / OBJECT_ID))
        creator = lt.create_torrent(files, piece, lt.create_torrent.v2_only)
        creator.set_priv(True)
        lt.set_piece_hashes(creator, str(source))
        self.torrent = lt.bencode(creator.generate())
        self.info_hash = str(lt.torrent_info(self.torrent).info_hashes().v2).lower()
        seed = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *args): pass
            def do_GET(self):
                if self.path.startswith("/api/torrent/"):
                    body, status, extra = seed.torrent, 200, {}
                else:
                    first, last = 0, seed.size - 1
                    ranged = self.headers.get("Range")
                    if ranged:
                        start, end = ranged.split("=", 1)[1].split("-", 1)
                        first, last = int(start), int(end or seed.size - 1)
                    body = seed.data[first:last + 1]
                    status = 206 if ranged else 200
                    extra = {"Content-Range": f"bytes {first}-{last}/{seed.size}"} if ranged else {}
                self.send_response(status)
                self.send_header("Content-Length", str(len(body)))
                for name, value in extra.items():
                    self.send_header(name, value)
                self.end_headers()
                try:
                    for offset in range(0, len(body), 32768):
                        self.wfile.write(body[offset:offset + 32768])
                except (BrokenPipeError, ConnectionResetError):
                    self.close_connection = True

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.origin = "http://127.0.0.1:%d" % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.counter = 0

    def grant(self):
        import base64
        self.counter += 1
        part = lambda n: base64.urlsafe_b64encode(os.urandom(n)).rstrip(b"=").decode()
        return part(9) + "." + part(64)

    def urls(self):
        return ("https://tracker.arknet.test/announce/" + self.grant(),
                self.origin + "/api/web-seed/v1/media/" + OBJECT_ID + "/" + self.grant(),
                self.origin + "/api/torrent/v1/media/" + OBJECT_ID + "/" + self.grant())

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class WebSeedTransferTests(unittest.TestCase):
    """Real libtorrent transfers from a local Range web seed (validation items 9 and 10)."""

    RATE = 512 * 1024

    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        self.seed = RangeSeed(self.root.name)
        self.managers = []

    def tearDown(self):
        for manager in self.managers:
            manager.close()
        self.seed.close()
        self.root.cleanup()

    def manager(self):
        manager = worker.Manager("https://tracker.arknet.test", Path(self.root.name) / "staging",
                                 Path(self.root.name) / "state", "127.0.0.1:0", self.RATE,
                                 web_seed_origin=self.seed.origin, allow_private_web_seed=True)
        self.managers.append(manager)
        return manager

    def add(self, manager):
        return manager.add(OBJECT_ID, self.seed.size, self.seed.info_hash, *self.seed.urls())

    def wait(self, manager, predicate, seconds, refresh_every=0.0):
        deadline = time.monotonic() + seconds
        next_refresh = time.monotonic() + refresh_every
        status = manager.get(OBJECT_ID)
        while time.monotonic() < deadline and not predicate(status):
            time.sleep(0.1)
            if refresh_every and time.monotonic() >= next_refresh:
                self.add(manager)  # what the Agent's reconciler does every interval
                next_refresh = time.monotonic() + refresh_every
            status = manager.get(OBJECT_ID)
        return status

    def assert_completed_exact_bytes(self, status):
        self.assertEqual(status["state"], "COMPLETED", status)
        payload = Path(self.root.name) / "staging" / OBJECT_ID / OBJECT_ID
        self.assertEqual(payload.read_bytes(), self.seed.data)

    def test_frequent_authorization_refresh_does_not_starve_web_seed_transfer(self):
        manager = self.manager()
        self.add(manager)
        status = self.wait(manager, lambda s: s["state"] == "COMPLETED", 40, refresh_every=0.25)
        self.assert_completed_exact_bytes(status)

    def test_restart_mid_download_restores_metadata_and_resumes_to_completion(self):
        manager = self.manager()
        self.add(manager)
        partial = self.wait(manager, lambda s: s["completed_bytes"] >= 512 * 1024, 20)
        self.assertLess(partial["completed_bytes"], self.seed.size)
        manager.close()
        self.managers.remove(manager)

        restarted = self.manager()
        self.assertEqual(restarted.get(OBJECT_ID)["state"], "PAUSED")
        self.add(restarted)  # Agent: refresh with fresh authorization, then resume
        self.assertTrue(restarted.entries[OBJECT_ID].handle.status().has_metadata,
                        "restored transfer must not wait for metadata no peer can provide")
        restarted.action(OBJECT_ID, "resume")
        status = self.wait(restarted, lambda s: s["state"] == "COMPLETED", 40, refresh_every=1.0)
        self.assert_completed_exact_bytes(status)


if __name__ == "__main__":
    unittest.main()
