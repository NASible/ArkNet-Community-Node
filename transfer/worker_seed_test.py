"""Seed-mode tests. They need the real libtorrent binding, so run them in the
transfer image (see transfer/README.md)."""
import hashlib
import importlib.util
import os
import tempfile
import time
import unittest
from pathlib import Path

import libtorrent as lt

SPEC = importlib.util.spec_from_file_location("arknet_worker_seed", Path(__file__).with_name("worker.py"))
worker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(worker)

OBJECT_ID = "media_" + "a" * 64
FILE_NAME = OBJECT_ID + ".mp4"
ORIGIN = "https://core.arknet.test"
TORRENT_URL = ORIGIN + "/api/torrent/v1/media/" + OBJECT_ID + "/YWJj.ZGVm"
TRACKER_ORIGIN = "https://tracker.arknet.test"
TRACKER = TRACKER_ORIGIN + "/announce/YWJj.ZGVm"
TRACKER_2 = TRACKER_ORIGIN + "/announce/Z2hp.amts"
SIZE = 3 * 16 * 1024 + 1234  # several pieces plus a short tail piece


def snapshot(directory):
    """Names, sizes, mtimes and digests: proves nothing in the media dir was written."""
    result = {}
    for path in sorted(Path(directory).iterdir()):
        stat = path.lstat()
        result[path.name] = (stat.st_size, stat.st_mtime_ns, hashlib.sha256(path.read_bytes()).hexdigest())
    return result


class SeedModeTests(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        root = Path(self.root.name)
        self.media, self.staging, self.state = root / "media", root / "staging", root / "state"
        self.media.mkdir()
        self.payload = os.urandom(SIZE)
        # Core's torrent names its single file by object ID (never the Node's file name).
        source = root / "source"
        source.mkdir()
        (source / OBJECT_ID).write_bytes(self.payload)
        files = lt.file_storage()
        files.add_file(OBJECT_ID, SIZE)
        creator = lt.create_torrent(files, 16 * 1024, lt.create_torrent.v2_only)
        creator.set_priv(True)
        lt.set_piece_hashes(creator, str(source))
        self.metadata = lt.bencode(creator.generate())
        self.info_hash = str(lt.torrent_info(self.metadata).info_hashes().v2).lower()
        self.fetched = []
        original = worker.fetch_torrent_metadata

        def fetch(url, limit=16 * 1024 * 1024):
            self.fetched.append(url)
            return self.metadata
        worker.fetch_torrent_metadata = fetch
        self.addCleanup(setattr, worker, "fetch_torrent_metadata", original)
        self.managers = []

    def tearDown(self):
        for manager in self.managers:
            manager.close()
        self.root.cleanup()

    def manager(self, media=True):
        value = worker.Manager(TRACKER_ORIGIN, str(self.staging), str(self.state), "127.0.0.1:0", 0, ORIGIN, False,
                               str(self.media) if media else "")
        self.managers.append(value)
        return value

    def write_media(self, data):
        (self.media / FILE_NAME).write_bytes(data)
        return snapshot(self.media)

    def wait_state(self, manager, wanted, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            seeds = {s["object_id"]: s for s in manager.list_seeds()}
            if seeds.get(OBJECT_ID, {}).get("state") == wanted:
                return seeds[OBJECT_ID]
            time.sleep(0.1)
        self.fail(f"seed did not reach {wanted}: {manager.list_seeds()}")

    def seed(self, manager, **overrides):
        values = dict(object_id=OBJECT_ID, size=SIZE, info_hash=self.info_hash, tracker_url=TRACKER,
                      torrent_url=TORRENT_URL, file_name=FILE_NAME)
        values.update(overrides)
        return manager.seed(**values)

    def test_valid_seed_is_hash_checked_then_serves_read_only_with_private_policy(self):
        before = self.write_media(self.payload)
        manager = self.manager()
        first = self.seed(manager)
        self.assertIn(first["state"], {"CHECKING", "SEEDING"})
        status = self.wait_state(manager, "SEEDING")
        self.assertEqual((status["total_bytes"], status["uploaded_bytes"]), (SIZE, 0))
        handle = manager.seeds[OBJECT_ID].handle
        self.assertEqual([t["url"] for t in handle.trackers()], [TRACKER])
        self.assertEqual(handle.url_seeds(), [])
        self.assertEqual(handle.http_seeds(), [])
        flags = handle.flags()
        for flag in (lt.torrent_flags.upload_mode, lt.torrent_flags.disable_dht, lt.torrent_flags.disable_lsd,
                     lt.torrent_flags.disable_pex):
            self.assertEqual(flags & flag, flag)
        self.assertFalse(flags & lt.torrent_flags.seed_mode)  # verified by recheck, not trusted
        self.assertTrue(handle.status().is_seeding)
        self.assertEqual(snapshot(self.media), before)
        self.assertEqual(manager.list(), [])  # seeds are not transfers
        self.assertEqual(list(self.state.iterdir()), [])  # and are never persisted

    def test_corrupt_file_fails_recheck_is_dropped_and_never_written(self):
        corrupt = bytearray(self.payload)
        corrupt[20000] ^= 0xFF
        before = self.write_media(bytes(corrupt))
        manager = self.manager()
        self.seed(manager)
        self.wait_state(manager, "VERIFICATION_FAILED")
        self.assertIsNone(manager.seeds[OBJECT_ID].handle)
        self.assertEqual(manager.session.get_torrents(), [])
        time.sleep(1.5)  # periodic refresh must not resurrect or repair it
        self.assertEqual(snapshot(self.media), before)

    def test_short_missing_or_non_regular_file_is_rejected_without_writing(self):
        manager = self.manager()
        with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
            self.seed(manager)  # missing: never created
        self.assertEqual(snapshot(self.media), {})
        before = self.write_media(self.payload[:-1])
        with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
            self.seed(manager)
        self.assertEqual(snapshot(self.media), before)
        (self.media / FILE_NAME).unlink()
        target = Path(self.root.name) / "elsewhere"
        target.write_bytes(self.payload)
        (self.media / FILE_NAME).symlink_to(target)
        with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
            self.seed(manager)
        self.assertEqual(manager.seeds, {})
        self.assertEqual(manager.session.get_torrents(), [])
        self.assertEqual(self.fetched, [])  # rejected before contacting Core

    def test_invalid_file_name_urls_hash_or_unconfigured_media_dir_are_rejected(self):
        self.write_media(self.payload)
        manager = self.manager()
        bad = [
            {"file_name": "../" + FILE_NAME}, {"file_name": OBJECT_ID + ".mkv"}, {"file_name": OBJECT_ID},
            {"file_name": "media_" + "c" * 64 + ".mp4"}, {"file_name": FILE_NAME.upper()},
            {"file_name": "/srv/arknet/media/" + FILE_NAME}, {"file_name": FILE_NAME + "\n"},
            {"tracker_url": "https://evil.test/announce/YWJj.ZGVm"}, {"tracker_url": TRACKER + "?x=1"},
            {"torrent_url": TORRENT_URL.replace(OBJECT_ID, "media_" + "c" * 64)},
            {"torrent_url": "https://evil.test/api/torrent/v1/media/" + OBJECT_ID + "/YWJj.ZGVm"},
            {"info_hash": "c" * 64},  # metadata for a different object
        ]
        for override in bad:
            with self.subTest(override=override):
                with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
                    self.seed(manager, **override)
        with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
            self.seed(self.manager(media=False))
        self.assertEqual(manager.seeds, {})

    def test_exact_protocol_for_seed_unseed_and_seeds(self):
        valid = {"id": 1, "op": "seed", "object_id": OBJECT_ID, "size": SIZE, "info_hash_v2": self.info_hash,
                 "tracker_url": TRACKER, "torrent_url": TORRENT_URL, "file_name": FILE_NAME}
        self.assertEqual(worker.exact_request(valid, TRACKER_ORIGIN, ORIGIN), "seed")
        for change in ({"web_seed_url": "x"}, {"file_name": "x.mp4"}, {"size": True}, {"size": -1},
                       {"tracker_url": "https://evil.test/announce/YWJj.ZGVm"}, {"extra": 1}):
            with self.subTest(change=change):
                with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
                    worker.exact_request({**valid, **change}, TRACKER_ORIGIN, ORIGIN)
        missing = dict(valid)
        del missing["file_name"]
        with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
            worker.exact_request(missing, TRACKER_ORIGIN, ORIGIN)
        self.assertEqual(worker.exact_request({"id": 2, "op": "unseed", "object_id": OBJECT_ID}), "unseed")
        self.assertEqual(worker.exact_request({"id": 3, "op": "seeds"}), "seeds")
        for value in ({"id": 2, "op": "unseed"}, {"id": 3, "op": "seeds", "object_id": OBJECT_ID}):
            with self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
                worker.exact_request(value)

    def test_dispatch_seed_lifecycle_and_unseed_removes_torrent_but_not_file(self):
        before = self.write_media(self.payload)
        manager = self.manager()
        seeded = worker.dispatch_request(manager, {
            "id": 7, "op": "seed", "object_id": OBJECT_ID, "size": SIZE, "info_hash_v2": self.info_hash,
            "tracker_url": TRACKER, "torrent_url": TORRENT_URL, "file_name": FILE_NAME}, TRACKER_ORIGIN, ORIGIN)
        self.assertEqual((seeded["id"], seeded["seed"]["object_id"]), (7, OBJECT_ID))
        self.wait_state(manager, "SEEDING")
        listed = worker.dispatch_request(manager, {"id": 8, "op": "seeds"})
        self.assertEqual([s["object_id"] for s in listed["seeds"]], [OBJECT_ID])
        removed = worker.dispatch_request(manager, {"id": 9, "op": "unseed", "object_id": OBJECT_ID})
        self.assertEqual(removed["seed"]["state"], "REMOVED")
        self.assertEqual(manager.session.get_torrents(), [])
        self.assertEqual(manager.list_seeds(), [])
        with self.assertRaisesRegex(worker.WorkerError, "not_found"):
            manager.unseed(OBJECT_ID)
        self.assertEqual(snapshot(self.media), before)

    def test_repeat_seed_refreshes_only_tracker_and_reannounces(self):
        self.write_media(self.payload)
        manager = self.manager()
        self.seed(manager)
        self.wait_state(manager, "SEEDING")
        handle = manager.seeds[OBJECT_ID].handle
        refreshed = self.seed(manager, tracker_url=TRACKER_2)
        self.assertEqual(refreshed["state"], "SEEDING")  # no second recheck
        self.assertIs(manager.seeds[OBJECT_ID].handle, handle)
        self.assertEqual([t["url"] for t in handle.trackers()], [TRACKER_2])
        self.assertEqual(handle.url_seeds(), [])
        self.assertEqual(len(self.fetched), 1)  # metadata is not fetched again
        self.assertEqual(len(manager.session.get_torrents()), 1)
        for override in ({"size": SIZE + 1}, {"info_hash": "c" * 64}):
            with self.assertRaisesRegex(worker.WorkerError, "conflict"):
                self.seed(manager, **override)

    def test_active_download_of_same_object_cannot_be_seeded(self):
        self.write_media(self.payload)
        manager = self.manager()
        manager.entries[OBJECT_ID] = worker.Entry({"object_id": OBJECT_ID, "size": SIZE,
                                                   "info_hash_v2": self.info_hash, "state": "PAUSED"})
        with self.assertRaisesRegex(worker.WorkerError, "conflict"):
            self.seed(manager)
        self.assertEqual(manager.seeds, {})

    def test_restart_leaves_no_seeds(self):
        before = self.write_media(self.payload)
        first = self.manager()
        self.seed(first)
        self.wait_state(first, "SEEDING")
        first.close()
        self.managers.remove(first)
        self.assertEqual(list(self.state.iterdir()), [])
        second = self.manager()
        self.assertEqual(second.list_seeds(), [])
        self.assertEqual(second.session.get_torrents(), [])
        self.assertEqual(second.list(), [])
        self.assertEqual(snapshot(self.media), before)


if __name__ == "__main__":
    unittest.main()
