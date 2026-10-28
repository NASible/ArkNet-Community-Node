#!/usr/bin/env python3
"""Libtorrent byte-movement worker for the local ArkNet transfer API."""
import base64
import json
import os
import re
import sys
import tempfile
import threading
import time
from pathlib import Path
from urllib.parse import urlsplit
import urllib.error
import urllib.request

import libtorrent as lt

ID_RE = re.compile(r"media_[0-9a-f]{64}\Z")
HASH_RE = re.compile(r"[0-9a-f]{64}\Z")
SEED_FILE_RE = re.compile(r"media_[0-9a-f]{64}\.mp4\Z")
SECURE_FLAGS = (lt.torrent_flags.disable_dht | lt.torrent_flags.disable_lsd |
                lt.torrent_flags.disable_pex | lt.torrent_flags.override_trackers |
                lt.torrent_flags.override_web_seeds)
TERMINAL = {"COMPLETED", "CANCELLED", "VERIFICATION_FAILED"}
CHECKING_STATES = {"queued_for_checking", "checking_files", "checking_resume_data", "allocating"}


def session_settings(listen_interfaces, rate_limit, allow_private_web_seed=False):
    return {
        "enable_dht": False,
        "enable_lsd": False,
        "enable_upnp": False,
        "enable_natpmp": False,
        "broadcast_lsd": False,
        "max_pex_peers": 0,
        "listen_interfaces": listen_interfaces,
        "download_rate_limit": rate_limit,
        "ignore_limits_on_local_network": False,
        "announce_to_all_trackers": False,
        "announce_to_all_tiers": False,
        "prefer_udp_trackers": False,
        "use_dht_as_fallback": False,
        "ssrf_mitigation": not allow_private_web_seed,
    }


def valid_web_seed_origin(origin, allow_http):
    if not origin:
        return not allow_http
    if len(origin) > 512 or any(char in origin for char in "%\\?#"):
        return False
    try:
        parsed = urlsplit(origin)
        if (not parsed.hostname or parsed.username is not None or parsed.password is not None or
                parsed.path or parsed.query or parsed.fragment or origin != parsed.scheme + "://" + parsed.netloc):
            return False
        if parsed.scheme != "https" and not (parsed.scheme == "http" and allow_http):
            return False
        if parsed.port is not None and not 1 <= parsed.port <= 65535:
            return False
        return True
    except (TypeError, ValueError):
        return False


def valid_web_seed_url(value, origin, object_id):
    if (not isinstance(value, str) or not value or not origin or len(value) > 2048 or
            any(char in value for char in "%\\")):
        return False
    try:
        parsed = urlsplit(value)
        if (parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment or
                parsed.scheme + "://" + parsed.netloc != origin):
            return False
        prefix = "/api/web-seed/v1/media/" + object_id + "/"
        if not parsed.path.startswith(prefix):
            return False
        token = parsed.path[len(prefix):]
        parts = token.split(".")
        if len(parts) != 2:
            return False
        for part in parts:
            if not part or not re.fullmatch(r"[A-Za-z0-9_-]+", part):
                return False
            padded = part + "=" * (-len(part) % 4)
            decoded = base64.b64decode(padded, altchars=b"-_", validate=True)
            if not decoded or base64.urlsafe_b64encode(decoded).rstrip(b"=").decode() != part:
                return False
        return True
    except (ValueError, TypeError):
        return False


def valid_tracker_url(value, origin):
    if (not isinstance(value, str) or not value or not origin or len(value) > 2048 or
            any(char in value for char in "%\\")):
        return False
    try:
        parsed = urlsplit(value)
        if (parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment or
                parsed.scheme + "://" + parsed.netloc != origin or
                not parsed.path.startswith("/announce/")):
            return False
        token = parsed.path[len("/announce/"):]
        parts = token.split(".")
        if len(parts) != 2:
            return False
        for part in parts:
            if not part or not re.fullmatch(r"[A-Za-z0-9_-]+", part):
                return False
            padded = part + "=" * (-len(part) % 4)
            decoded = base64.b64decode(padded, altchars=b"-_", validate=True)
            if not decoded or base64.urlsafe_b64encode(decoded).rstrip(b"=").decode() != part:
                return False
        return True
    except (ValueError, TypeError):
        return False


def valid_torrent_url(value, origin, object_id):
    if (not isinstance(value, str) or not value or not origin or len(value) > 2048 or
            any(char in value for char in "%\\")):
        return False
    try:
        parsed = urlsplit(value)
        if (parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment or
                parsed.scheme + "://" + parsed.netloc != origin):
            return False
        prefix = "/api/torrent/v1/media/" + object_id + "/"
        if not parsed.path.startswith(prefix):
            return False
        token = parsed.path[len(prefix):]
        parts = token.split(".")
        if len(parts) != 2:
            return False
        for part in parts:
            if not part or not re.fullmatch(r"[A-Za-z0-9_-]+", part):
                return False
            padded = part + "=" * (-len(part) % 4)
            decoded = base64.b64decode(padded, altchars=b"-_", validate=True)
            if not decoded or base64.urlsafe_b64encode(decoded).rstrip(b"=").decode() != part:
                return False
        return True
    except (ValueError, TypeError):
        return False


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def fetch_torrent_metadata(value, limit=16 * 1024 * 1024):
    request = urllib.request.Request(value, headers={"Accept": "application/x-bittorrent"})
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect).open(request, timeout=10) as response:
            if response.status != 200:
                raise ValueError("metadata unavailable")
            data = response.read(limit + 1)
    except (OSError, urllib.error.URLError, urllib.error.HTTPError) as error:
        raise ValueError("metadata unavailable") from error
    if not data or len(data) > limit:
        raise ValueError("metadata unavailable")
    return data


def new_params(object_id, size, info_hash, tracker, staging, web_seed_url, torrent_data):
    info = lt.torrent_info(torrent_data)
    if not valid_metadata(info, info_hash, size):
        raise ValueError("invalid torrent metadata")
    params = lt.add_torrent_params()
    params.ti = info
    params.save_path = str(Path(staging) / object_id)
    params.trackers = [tracker]
    params.url_seeds = [web_seed_url] if web_seed_url else []
    params.http_seeds = []
    params.flags |= SECURE_FLAGS
    params.flags &= ~lt.torrent_flags.paused
    params.name = object_id
    return params


def valid_metadata(info, expected_hash, expected_size):
    try:
        hashes = info.info_hashes()
        files = info.files()
        payload = []
        for index in range(files.num_files()):
            flags = files.file_flags(index)
            if flags & lt.file_storage.flag_pad_file:
                continue
            payload.append((files.file_path(index), int(files.file_size(index))))
        return (info.priv() and hashes.has_v2() and not hashes.has_v1() and
                str(hashes.v2).lower() == expected_hash and len(payload) == 1 and
                payload[0][1] == expected_size)
    except Exception:
        return False


def metadata_diagnostic(info, expected_hash, expected_size):
    """Return non-secret rejection facts suitable for local structured logs."""
    try:
        hashes = info.info_hashes()
        files = info.files()
        sizes = []
        for index in range(files.num_files()):
            if not files.file_flags(index) & lt.file_storage.flag_pad_file:
                sizes.append(int(files.file_size(index)))
        return {"event": "metadata_rejected", "private": bool(info.priv()),
                "has_v1": bool(hashes.has_v1()), "has_v2": bool(hashes.has_v2()),
                "hash_match": str(hashes.v2).lower() == expected_hash,
                "payload_sizes": sizes, "expected_size": expected_size}
    except Exception:
        return {"event": "metadata_rejected", "inspection_failed": True}


def has_torrent_error(status):
    error = getattr(status, "errc", None)
    if error is None:
        return False
    value = getattr(error, "value", None)
    return value() != 0 if callable(value) else bool(error)


def atomic_write(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=".arknet-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


# Keep a live, previously authorized URL seed for this long before rotating to a
# fresh grant. Must stay well inside Core's 5-minute grant lifetime.
WEB_SEED_ROTATE_SECONDS = 120.0


class Entry:
    def __init__(self, manifest, handle=None, tracker_url="", web_seed_url="", torrent_data=b""):
        self.applied_web_seed = None  # runtime only: (handle, url, monotonic time); never persisted
        self.object_id = manifest["object_id"]
        self.size = manifest["size"]
        self.info_hash = manifest["info_hash_v2"]
        self.state = manifest["state"]
        self.error_code = manifest.get("error_code", "")
        self.handle = handle
        self.secured_handle = None  # runtime only; never persisted
        self.metadata_validated = manifest.get("metadata_validated", False)
        self.completed_bytes = manifest.get("completed_bytes", 0)
        self.last_resume = 0.0
        self.progress_diagnostic_emitted = False
        self.tracker_url = tracker_url
        self.web_seed_url = web_seed_url
        self.torrent_data = torrent_data

    def manifest(self):
        return {
            "object_id": self.object_id,
            "size": self.size,
            "info_hash_v2": self.info_hash,
            "state": self.state,
            "error_code": self.error_code,
            "metadata_validated": self.metadata_validated,
            "completed_bytes": self.completed_bytes,
        }


def valid_seed_file_name(value, object_id):
    """A seed serves exactly the Agent-finalized file for its own object."""
    return isinstance(value, str) and bool(SEED_FILE_RE.fullmatch(value)) and value == object_id + ".mp4"


class Seed:
    """Runtime-only serving state. Never persisted: a restart serves nothing
    until the Agent re-authorizes each seed with a fresh Core seed grant."""

    def __init__(self, object_id, size, info_hash, handle):
        self.object_id = object_id
        self.size = size
        self.info_hash = info_hash
        self.handle = handle
        self.state = "CHECKING"

    def status(self, uploaded=0):
        return {"object_id": self.object_id, "state": self.state, "total_bytes": self.size,
                "uploaded_bytes": uploaded}


class Manager:
    def __init__(self, tracker_origin, staging, state_dir, listen_interfaces, rate_limit,
                 web_seed_origin="", allow_private_web_seed=False, media_dir=""):
        self.tracker_origin = tracker_origin
        self.media_dir = Path(media_dir) if media_dir else None
        self.seeds = {}
        self.staging = Path(staging)
        self.state_dir = Path(state_dir)
        self.web_seed_origin = web_seed_origin
        self.staging.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.session = lt.session(session_settings(listen_interfaces, rate_limit,
                                                   allow_private_web_seed))
        # Metadata exchange is required for v2 magnets. Deliberately never install ut_pex.
        self.session.add_extension(lt.create_ut_metadata_plugin)
        self.entries = {}
        self.lock = threading.RLock()
        self.stopping = threading.Event()
        self._restore()
        self.saver = threading.Thread(target=self._periodic_save, name="resume-writer", daemon=True)
        self.saver.start()

    def manifest_path(self, object_id):
        return self.state_dir / (object_id + ".json")

    def resume_path(self, object_id):
        return self.state_dir / (object_id + ".fastresume")

    def _write_manifest(self, entry):
        atomic_write(self.manifest_path(entry.object_id),
                     json.dumps(entry.manifest(), sort_keys=True, separators=(",", ":")).encode())

    def _sanitized_params(self, entry):
        # The Core-fetched metadata is authoritative. Legacy resume data from the
        # Ubuntu 24.04 binding carries no info dict, and a web seed cannot supply
        # metadata, so a resumed handle without it would wait forever. Validate it
        # before touching resume data so bad metadata never discards progress.
        info = lt.torrent_info(entry.torrent_data)
        if not valid_metadata(info, entry.info_hash, entry.size):
            raise ValueError("invalid torrent metadata")
        params = None
        resume = self.resume_path(entry.object_id)
        if resume.exists():
            try:
                params = lt.read_resume_data(resume.read_bytes())
                hashes = params.info_hashes
                if not hashes.has_v2() or hashes.has_v1() or str(hashes.v2).lower() != entry.info_hash:
                    raise ValueError("resume identity mismatch")
            except Exception:
                resume.unlink(missing_ok=True)
                params = None
        if params is None:
            params = new_params(entry.object_id, entry.size, entry.info_hash, entry.tracker_url, self.staging,
                                entry.web_seed_url, entry.torrent_data)
        params.ti = info
        params.save_path = str(self.staging / entry.object_id)
        params.trackers = [entry.tracker_url]
        params.url_seeds = [entry.web_seed_url] if entry.web_seed_url else []
        params.http_seeds = []
        params.flags |= SECURE_FLAGS
        params.name = entry.object_id
        return params

    def _restore(self):
        for path in sorted(self.state_dir.glob("media_*.json")):
            try:
                value = json.loads(path.read_text())
                if (set(value) - {"object_id", "size", "info_hash_v2", "state", "error_code", "metadata_validated", "completed_bytes"} or
                        not ID_RE.fullmatch(value["object_id"]) or not HASH_RE.fullmatch(value["info_hash_v2"]) or
                        not isinstance(value["size"], int) or value["size"] < 0 or
                        value["state"] not in {"DOWNLOADING", "PAUSED", *TERMINAL}):
                    continue
                entry = Entry(value)
                if entry.state not in TERMINAL:
                    entry.state = "PAUSED"
                    self._write_manifest(entry)
                self.entries[entry.object_id] = entry
            except Exception:
                continue

    def _fail(self, entry):
        if entry.handle is not None:
            try:
                entry.handle.pause()
                self.session.remove_torrent(entry.handle)
            except Exception:
                pass
            entry.handle = None
        entry.state = "VERIFICATION_FAILED"
        entry.error_code = "verification_failed"
        self._write_manifest(entry)

    def _secure_metadata(self, entry):
        # Secure each libtorrent handle once. Re-applying sources on every refresh
        # disconnects the in-flight web-seed request and wastes Core egress.
        if entry.secured_handle is not None and entry.secured_handle is entry.handle:
            return True
        status = entry.handle.status()
        if not status.has_metadata:
            return True
        info = entry.handle.torrent_file()
        if not valid_metadata(info, entry.info_hash, entry.size):
            print(json.dumps(metadata_diagnostic(info, entry.info_hash, entry.size),
                             separators=(",", ":")), file=sys.stderr, flush=True)
            self._fail(entry)
            return False
        try:
            self._apply_authorization(entry)
        except Exception as error:
            print(json.dumps({"event": "authorization_apply_failed", "error_type": type(error).__name__},
                             separators=(",", ":")), file=sys.stderr, flush=True)
            self._fail(entry)
            return False
        entry.metadata_validated = True
        entry.secured_handle = entry.handle
        return True

    def _refresh(self, entry, persist=False):
        if entry.state in TERMINAL or entry.handle is None:
            return
        try:
            if not self._secure_metadata(entry):
                return
            status = entry.handle.status()
            if has_torrent_error(status):
                print(json.dumps({"event": "torrent_status_error"}, separators=(",", ":")),
                      file=sys.stderr, flush=True)
                self._fail(entry)
                return
            wanted_done = int(getattr(status, "total_wanted_done", status.total_done))
            entry.completed_bytes = max(0, min(entry.size, wanted_done))
            if entry.metadata_validated and entry.completed_bytes == 0 and not entry.progress_diagnostic_emitted:
                print(json.dumps({"event": "transfer_waiting_for_source",
                                  "num_peers": int(getattr(status, "num_peers", 0)),
                                  "num_seeds": int(getattr(status, "num_seeds", 0)),
                                  "download_rate": int(getattr(status, "download_rate", 0)),
                                  "total_wanted": int(getattr(status, "total_wanted", 0))},
                                 separators=(",", ":")), file=sys.stderr, flush=True)
                entry.progress_diagnostic_emitted = True
            if status.is_seeding:
                if not entry.metadata_validated:
                    self._fail(entry)
                    return
                entry.completed_bytes = entry.size
                entry.state = "COMPLETED"
                entry.error_code = ""
                self._save_resume(entry, force=True)
                self._write_manifest(entry)
                # Leave the swarm: receiving never implies serving. Without this the
                # handle keeps seeding (even after the Agent moves the file, via the
                # open descriptor), regardless of the operator's Peer Serving setting.
                self.session.remove_torrent(entry.handle)
                entry.handle = None
                entry.secured_handle = None
                return
            entry.state = "PAUSED" if entry.handle.is_paused() else "DOWNLOADING"
            if persist:
                self._save_resume(entry)
                self._write_manifest(entry)
        except Exception as error:
            print(json.dumps({"event": "transfer_refresh_failed", "error_type": type(error).__name__},
                             separators=(",", ":")), file=sys.stderr, flush=True)
            self._fail(entry)

    def _save_resume(self, entry, force=False):
        if entry.handle is None:
            return
        now = time.monotonic()
        if not force and now - entry.last_resume < 1.0:
            return
        try:
            # Ubuntu 24.04's libtorrent binding returns the legacy resume dict.
            resume = entry.handle.write_resume_data()
            if not isinstance(resume, dict):
                raise TypeError("unexpected resume data")
            for key in ("url-list", b"url-list", "httpseeds", b"httpseeds",
                        "url_seeds", b"url_seeds", "http_seeds", b"http_seeds",
                        "trackers", b"trackers", "tracker", b"tracker",
                        "announce", b"announce", "announce-list", b"announce-list"):
                resume.pop(key, None)
            data = lt.bencode(resume)
            atomic_write(self.resume_path(entry.object_id), data)
            entry.last_resume = now
        except Exception:
            if force:
                raise

    def _periodic_save(self):
        while not self.stopping.wait(1.0):
            with self.lock:
                for entry in self.entries.values():
                    self._refresh(entry, persist=True)
                for seed in list(self.seeds.values()):
                    self._refresh_seed(seed)

    def _refresh_seed(self, seed):
        """Promote a fully verified seed to SEEDING; drop one whose recheck fails."""
        if seed.handle is None:
            return 0
        try:
            status = seed.handle.status()
            if has_torrent_error(status):
                raise ValueError("seed error")
            state = str(getattr(status, "state", ""))
            if any(name in state for name in CHECKING_STATES):
                seed.state = "CHECKING"
            elif status.is_seeding and int(status.total_wanted_done) == seed.size:
                seed.state = "SEEDING"
            else:
                raise ValueError("local file does not match the authorized object")
            return int(getattr(status, "all_time_upload", 0))
        except Exception:
            # Never serve unverified bytes; upload mode guarantees nothing was written.
            try:
                self.session.remove_torrent(seed.handle)
            except Exception:
                pass
            seed.handle = None
            seed.state = "VERIFICATION_FAILED"
            print(json.dumps({"event": "seed_verification_failed"}, separators=(",", ":")),
                  file=sys.stderr, flush=True)
            return 0

    def seed(self, object_id, size, info_hash, tracker_url, torrent_url, file_name):
        with self.lock:
            if (self.media_dir is None or not valid_tracker_url(tracker_url, self.tracker_origin) or
                    not valid_torrent_url(torrent_url, self.web_seed_origin, object_id) or
                    not valid_seed_file_name(file_name, object_id)):
                raise WorkerError("invalid_request")
            entry = self.entries.get(object_id)
            if entry is not None and entry.state not in TERMINAL:
                raise WorkerError("conflict")  # still receiving: never serve partial data
            existing = self.seeds.get(object_id)
            if existing is not None:
                if existing.size != size or existing.info_hash != info_hash:
                    raise WorkerError("conflict")
                if existing.handle is not None:
                    # Fresh grant: refresh only the announce URL, then re-announce.
                    existing.handle.replace_trackers([lt.announce_entry(tracker_url)])
                    existing.handle.force_reannounce()
                return existing.status(self._refresh_seed(existing))
            path = self.media_dir / file_name
            try:
                info = os.lstat(path)
            except OSError:
                raise WorkerError("invalid_request")
            if not (info.st_mode & 0o170000 == 0o100000) or info.st_size != size:
                raise WorkerError("invalid_request")
            try:
                metadata = lt.torrent_info(fetch_torrent_metadata(torrent_url))
            except Exception:
                raise WorkerError("invalid_request")
            if not valid_metadata(metadata, info_hash, size):
                raise WorkerError("invalid_request")
            files = metadata.files()
            payload = [i for i in range(files.num_files()) if not files.file_flags(i) & lt.file_storage.flag_pad_file]
            params = lt.add_torrent_params()
            params.ti = metadata
            params.save_path = str(self.media_dir)
            params.renamed_files = {payload[0]: file_name}
            params.trackers = [tracker_url]
            params.url_seeds = []
            params.http_seeds = []
            params.name = object_id
            # No resume data and no seed_mode: libtorrent hash-checks every piece
            # before serving. Upload mode means a seed can never download or write.
            params.flags |= SECURE_FLAGS | lt.torrent_flags.upload_mode
            params.flags &= ~(lt.torrent_flags.paused | lt.torrent_flags.auto_managed |
                              lt.torrent_flags.seed_mode | lt.torrent_flags.share_mode)
            try:
                handle = self.session.add_torrent(params)
            except Exception:
                raise WorkerError("invalid_request")
            seed = Seed(object_id, size, info_hash, handle)
            self.seeds[object_id] = seed
            return seed.status(self._refresh_seed(seed))

    def unseed(self, object_id):
        with self.lock:
            seed = self.seeds.pop(object_id, None)
            if seed is None:
                raise WorkerError("not_found")
            if seed.handle is not None:
                self.session.remove_torrent(seed.handle)  # never delete files
                seed.handle = None
            return {"object_id": object_id, "state": "REMOVED", "total_bytes": seed.size, "uploaded_bytes": 0}

    def list_seeds(self):
        with self.lock:
            return [self.seeds[key].status(self._refresh_seed(self.seeds[key])) for key in sorted(self.seeds)]

    def status(self, entry):
        self._refresh(entry, persist=True)
        return {
            "object_id": entry.object_id,
            "state": entry.state,
            "total_bytes": entry.size,
            "completed_bytes": entry.completed_bytes,
            "mock": False,
            "error_code": entry.error_code,
        }

    def _apply_authorization(self, entry, now=None):
        """Install the current grant on the live handle.

        Trackers are replaced every time. The URL seed is kept only while it is
        exactly the URL this worker authorized on this same handle and younger
        than WEB_SEED_ROTATE_SECONDS: removing it aborts libtorrent's in-flight
        range request, which covers the rest of the file, and discards its data.
        """
        now = time.monotonic() if now is None else now
        handle = entry.handle
        handle.replace_trackers([lt.announce_entry(entry.tracker_url)])
        for url in list(handle.http_seeds()):
            handle.remove_http_seed(url)
        live = list(handle.url_seeds())
        applied = entry.applied_web_seed
        if (entry.web_seed_url and applied is not None and applied[0] is handle and live == [applied[1]] and
                now - applied[2] < WEB_SEED_ROTATE_SECONDS):
            return
        for url in live:
            handle.remove_url_seed(url)
        entry.applied_web_seed = None
        if entry.web_seed_url:
            handle.add_url_seed(entry.web_seed_url)
            entry.applied_web_seed = (handle, entry.web_seed_url, now)

    def add(self, object_id, size, info_hash, tracker_url, web_seed_url, torrent_url):
        with self.lock:
            if (not valid_tracker_url(tracker_url, self.tracker_origin) or
                    not valid_web_seed_url(web_seed_url, self.web_seed_origin, object_id) or
                    not valid_torrent_url(torrent_url, self.web_seed_origin, object_id)):
                raise WorkerError("invalid_request")
            existing = self.entries.get(object_id)
            if existing:
                if existing.size != size or existing.info_hash != info_hash:
                    raise WorkerError("conflict")
                if existing.state not in TERMINAL:
                    try:
                        existing.tracker_url = tracker_url
                        existing.web_seed_url = web_seed_url
                        if existing.handle is None:
                            existing.torrent_data = fetch_torrent_metadata(torrent_url)
                            existing.handle = self.session.add_torrent(self._sanitized_params(existing))
                            existing.handle.pause()
                            existing.state = "PAUSED"
                        self._apply_authorization(existing)
                    except Exception:
                        raise WorkerError("invalid_request")
                return self.status(existing)
            manifest = {"object_id": object_id, "size": size, "info_hash_v2": info_hash,
                        "state": "DOWNLOADING", "error_code": "", "completed_bytes": 0}
            try:
                torrent_data = fetch_torrent_metadata(torrent_url)
            except ValueError:
                raise WorkerError("invalid_request")
            entry = Entry(manifest, tracker_url=tracker_url, web_seed_url=web_seed_url,
                          torrent_data=torrent_data)
            try:
                entry.handle = self.session.add_torrent(self._sanitized_params(entry))
            except Exception:
                raise WorkerError("invalid_request")
            self.entries[object_id] = entry
            self._write_manifest(entry)
            return self.status(entry)

    def get(self, object_id):
        with self.lock:
            if object_id not in self.entries:
                raise WorkerError("not_found")
            return self.status(self.entries[object_id])

    def list(self):
        with self.lock:
            return [self.status(self.entries[key]) for key in sorted(self.entries)]

    def action(self, object_id, action):
        with self.lock:
            entry = self.entries.get(object_id)
            if entry is None:
                raise WorkerError("not_found")
            if action == "cancel":
                if entry.state == "CANCELLED":
                    return self.status(entry)
                if entry.state in TERMINAL:
                    raise WorkerError("invalid_transition")
                if entry.handle is not None:
                    self._save_resume(entry, force=True)
                    self.session.remove_torrent(entry.handle)
                    entry.handle = None
                entry.state = "CANCELLED"
                entry.error_code = ""
            elif action == "pause":
                if entry.state == "PAUSED":
                    return self.status(entry)
                if entry.state != "DOWNLOADING" or entry.handle is None:
                    raise WorkerError("invalid_transition")
                entry.handle.pause()
                entry.state = "PAUSED"
                self._save_resume(entry, force=True)
            elif action == "resume":
                if entry.state == "DOWNLOADING":
                    return self.status(entry)
                if entry.state != "PAUSED" or entry.handle is None:
                    raise WorkerError("invalid_transition")
                entry.handle.resume()
                entry.state = "DOWNLOADING"
            else:
                raise WorkerError("invalid_request")
            self._write_manifest(entry)
            return self.status(entry)

    def close(self):
        self.stopping.set()
        self.saver.join(timeout=3)
        with self.lock:
            self.session.pause()
            for seed in self.seeds.values():
                if seed.handle is not None:
                    self.session.remove_torrent(seed.handle)
                    seed.handle = None
            self.seeds.clear()
            for entry in self.entries.values():
                if entry.handle is not None:
                    self._refresh(entry)
                    self._save_resume(entry, force=True)
                    self._write_manifest(entry)


class WorkerError(Exception):
    pass


def exact_request(value, tracker_origin="", web_seed_origin=""):
    if not isinstance(value, dict) or not isinstance(value.get("id"), int) or value["id"] < 1:
        raise WorkerError("invalid_request")
    operation = value.get("op")
    keys = {"id", "op"}
    if operation == "add":
        keys |= {"object_id", "size", "info_hash_v2", "tracker_url", "web_seed_url", "torrent_url"}
    elif operation == "seed":
        keys |= {"object_id", "size", "info_hash_v2", "tracker_url", "torrent_url", "file_name"}
    elif operation in {"get", "pause", "resume", "cancel", "unseed"}:
        keys.add("object_id")
    elif operation not in {"list", "seeds", "shutdown"}:
        raise WorkerError("invalid_request")
    if set(value) != keys:
        raise WorkerError("invalid_request")
    if "object_id" in value and (not isinstance(value["object_id"], str) or not ID_RE.fullmatch(value["object_id"])):
        raise WorkerError("invalid_request")
    if operation == "seed":
        if (not isinstance(value["size"], int) or isinstance(value["size"], bool) or value["size"] < 0 or
                not isinstance(value["info_hash_v2"], str) or not HASH_RE.fullmatch(value["info_hash_v2"]) or
                not isinstance(value["tracker_url"], str) or
                not valid_tracker_url(value["tracker_url"], tracker_origin) or
                not isinstance(value["torrent_url"], str) or
                not valid_torrent_url(value["torrent_url"], web_seed_origin, value["object_id"]) or
                not valid_seed_file_name(value["file_name"], value["object_id"])):
            raise WorkerError("invalid_request")
    if operation == "add":
        if (not isinstance(value["size"], int) or isinstance(value["size"], bool) or value["size"] < 0 or
                not isinstance(value["info_hash_v2"], str) or not HASH_RE.fullmatch(value["info_hash_v2"]) or
                not isinstance(value["tracker_url"], str) or
                not valid_tracker_url(value["tracker_url"], tracker_origin) or
                not isinstance(value["web_seed_url"], str) or
                not valid_web_seed_url(value["web_seed_url"], web_seed_origin, value["object_id"]) or
                not isinstance(value["torrent_url"], str) or
                not valid_torrent_url(value["torrent_url"], web_seed_origin, value["object_id"])):
            raise WorkerError("invalid_request")
    return operation


def dispatch_request(manager, value, tracker_origin="", web_seed_origin=""):
    operation = exact_request(value, tracker_origin, web_seed_origin)
    request_id = value["id"]
    if operation == "shutdown":
        manager.close()
        return {"id": request_id, "ok": True}
    if operation == "add":
        result = {"status": manager.add(value["object_id"], value["size"],
                                        value["info_hash_v2"], value["tracker_url"],
                                        value["web_seed_url"], value["torrent_url"])}
    elif operation == "seed":
        result = {"seed": manager.seed(value["object_id"], value["size"], value["info_hash_v2"],
                                       value["tracker_url"], value["torrent_url"], value["file_name"])}
    elif operation == "unseed":
        result = {"seed": manager.unseed(value["object_id"])}
    elif operation == "seeds":
        result = {"seeds": manager.list_seeds()}
    elif operation == "list":
        result = {"transfers": manager.list()}
    elif operation == "get":
        result = {"status": manager.get(value["object_id"])}
    else:
        result = {"status": manager.action(value["object_id"], operation)}
    result["id"] = request_id
    return result


def main():
    tracker_origin = os.environ["ARKNET_TRANSFER_TRACKER_ORIGIN"]
    web_seed_origin = os.environ.get("ARKNET_TRANSFER_WEB_SEED_ORIGIN", "")
    tracker_allow_http = os.environ.get("ARKNET_TRANSFER_TRACKER_ALLOW_HTTP", "false")
    allow_http_value = os.environ.get("ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP", "false")
    if (tracker_allow_http not in {"true", "false"} or
            not valid_web_seed_origin(tracker_origin, tracker_allow_http == "true")):
        raise RuntimeError("invalid tracker origin policy")
    if (allow_http_value not in {"true", "false"} or
            not valid_web_seed_origin(web_seed_origin, allow_http_value == "true")):
        raise RuntimeError("invalid web-seed origin policy")
    manager = Manager(tracker_origin, os.environ["ARKNET_TRANSFER_STAGING_DIR"],
                      os.environ["ARKNET_TRANSFER_STATE_DIR"],
                      os.environ.get("ARKNET_TRANSFER_LISTEN_INTERFACES", "0.0.0.0:0"),
                      int(os.environ.get("ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT", "0")),
                      web_seed_origin, allow_http_value == "true",
                      os.environ.get("ARKNET_TRANSFER_MEDIA_DIR", ""))
    print(json.dumps({"ready": True, "backend": "libtorrent", "version": lt.version,
                      "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": False}},
                     separators=(",", ":")), flush=True)
    try:
        while True:
            line = sys.stdin.buffer.readline(1 << 20)
            if not line:
                break
            request_id = None
            try:
                if len(line) >= 1 << 20 or not line.endswith(b"\n"):
                    raise WorkerError("invalid_request")
                value = json.loads(line)
                request_id = value.get("id") if isinstance(value, dict) else None
                operation = exact_request(value, tracker_origin, web_seed_origin)
                result = dispatch_request(manager, value, tracker_origin, web_seed_origin)
                if operation == "shutdown":
                    print(json.dumps(result, separators=(",", ":")), flush=True)
                    return
            except WorkerError as error:
                result = {"id": request_id, "error": str(error)}
            except Exception:
                result = {"id": request_id, "error": "invalid_request"}
            print(json.dumps(result, separators=(",", ":")), flush=True)
    finally:
        manager.close()


if __name__ == "__main__":
    main()
