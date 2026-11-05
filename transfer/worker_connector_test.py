"""Worker-to-connector wiring and strict relay metadata validation."""
import threading
import time
import unittest
import worker


class RecordingConnector:
    def __init__(self):
        self.calls = []
    def authorize(self, *args, **kwargs):
        self.calls.append((args, kwargs))
    def tracker_url(self, info_hash, original):
        return "http://127.0.0.1:19081/announce/local"
    def remove(self, info_hash):
        self.calls.append(((info_hash,), {"remove": True}))


class ConnectorWiringTests(unittest.TestCase):
    def test_verified_relay_metadata_reaches_connector_without_changing_origin(self):
        manager = worker.Manager.__new__(worker.Manager)
        manager.connector = RecordingConnector()
        relays = [{"id": "relay-dc-1", "address": "relay.test:7443", "spki_sha256": "a" * 64}]
        expiry = int(time.time()) + 60
        manager._authorize_relay("b" * 64, "https://tracker.test/announce/grant", relays, expiry, seed=False)
        self.assertEqual(manager.connector.calls, [(("b" * 64, "https://tracker.test/announce/grant", relays, expiry), {"seed": False})])
        self.assertEqual(manager._local_tracker("b" * 64, "https://tracker.test/announce/grant"), "http://127.0.0.1:19081/announce/local")

    def test_connector_refusal_is_invalid_request_with_only_a_fixed_reason_logged(self):
        import contextlib, io, json

        class Refusing(RecordingConnector):
            def __init__(self, message):
                super().__init__()
                self.message = message
            def authorize(self, *args, **kwargs):
                raise ValueError(self.message)

        secret = "https://tracker.test/announce/SECRETGRANT.SIG"
        for message, logged in (("expired grant", "expired grant"),
                                ("unexpected " + secret, "other")):
            with self.subTest(message=message):
                manager = worker.Manager.__new__(worker.Manager)
                manager.connector = Refusing(message)
                err = io.StringIO()
                with contextlib.redirect_stderr(err), self.assertRaisesRegex(worker.WorkerError, "invalid_request"):
                    manager._authorize_relay("b" * 64, secret, [], 0, seed=False)
                self.assertEqual(json.loads(err.getvalue()), {"event": "relay_authorization_refused", "reason": logged})
                self.assertNotIn("SECRET", err.getvalue())

    def test_relay_metadata_absent_is_webseed_only_and_malformed_is_refused(self):
        self.assertTrue(worker.valid_relays([], 0))
        relay = {"id": "r", "address": "relay.test:7443", "spki_sha256": "a" * 64}
        expiry = int(time.time()) + 60
        self.assertTrue(worker.valid_relays([relay], expiry))
        for relays, end in (([relay], 0), ([relay], int(time.time())), ([relay] * 17, expiry),
                            ([{**relay, "address": "peer.test"}], expiry),
                            ([{**relay, "spki_sha256": "bad"}], expiry),
                            ([{**relay, "unknown": True}], expiry), ([relay, relay], expiry)):
            with self.subTest(relays=relays, expiry=end):
                self.assertFalse(worker.valid_relays(relays, end))


if __name__ == "__main__":
    unittest.main()
