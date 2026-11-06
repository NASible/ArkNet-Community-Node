"""Exercise the E2E playback helper without starting Docker."""
import ast
import hashlib
from pathlib import Path
import unittest
import urllib.parse


class JellyfinStreamTests(unittest.TestCase):
    def test_playback_does_not_select_an_unrelated_movie(self):
        source = Path(__file__).with_name("check-mvp-e2e.py").read_text()
        function = next(n for n in ast.walk(ast.parse(source))
                        if isinstance(n, ast.FunctionDef) and n.name == "jellyfin_stream")
        payload = b"the retained movie"
        streams = []
        target = "media_" + "a" * 64

        def request_json(url, *args, **kwargs):
            return 200, {}, {"Items": [
                {"Id": "unrelated", "Type": "Movie", "Path": "/srv/arknet/media/media_other.mp4"},
                {"Id": "expected", "Type": "Movie", "Path": "/srv/arknet/media/" + target + ".mp4"},
            ]}

        def request(url, **kwargs):
            streams.append(url)
            return 200, {}, payload if "/Videos/expected/" in url else b"another movie"

        namespace = {"nodes": {"a": {"jelly_access": "test", "jelly_base": "http://jelly", "jelly_key": "test"}},
                     "request_json": request_json, "request": request, "hashlib": hashlib,
                     "urllib": urllib, "wait_until": lambda name, probe, timeout: probe()}
        exec(compile(ast.Module(body=[function], type_ignores=[]), "playback-helper", "exec"), namespace)
        self.assertTrue(namespace["jellyfin_stream"]("a", target, hashlib.sha256(payload).hexdigest(), len(payload)))
        self.assertEqual(len(streams), 1)
        self.assertIn("/Videos/expected/", streams[0])


if __name__ == "__main__":
    unittest.main()
