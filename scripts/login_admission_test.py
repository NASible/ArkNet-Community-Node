"""Login admission retry regressions; no Core/Docker/VM required."""
import runpy
from pathlib import Path
import unittest
import urllib.request
from unittest import mock

from scripts import global_login as gl
from scripts import rate_limit_retry


class LoginAdmissionTests(unittest.TestCase):
    def test_browser_reposts_same_login_form_only_after_429(self):
        browser = mock.Mock()
        browser.request.side_effect = [
            (200, {}, None, b'<input name="password">'),
            (429, {"Retry-After": "6"}, None, b"limited"),
            (303, {"Location": "http://node/callback#assertion=proof"}, None, b""),
        ]
        with mock.patch.object(rate_limit_retry.time, "sleep") as sleep:
            result = gl.core_sign_in(browser, "http://core/node-login?node_id=n&nonce=x", "alice", "pw")
        self.assertEqual(result, (303, "proof", "http://node/callback#assertion=proof"))
        self.assertEqual(browser.request.call_args_list[1], browser.request.call_args_list[2])
        sleep.assert_called_once_with(6.0)

    def test_api_login_shared_request_retries_429_but_returns_401(self):
        module = runpy.run_path(str(Path(__file__).with_name("check-core-to-node-transfer-stack.py")))
        requests = []

        def open_request(req, timeout):
            requests.append(req)
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.status = 429 if len(requests) == 1 else 401
            response.headers = {"Retry-After": "6"}
            response.read.return_value = b"unchanged"
            return response

        with mock.patch.object(urllib.request, "urlopen", side_effect=open_request), \
                mock.patch.object(rate_limit_retry.time, "sleep") as sleep:
            result = module["request"]("http://core/api/identity/v1/sessions", "POST",
                                       {"username": "alice", "password": "wrong"})
        self.assertEqual(result[0], 401)
        self.assertEqual(len(requests), 2)
        self.assertEqual(requests[0].data, requests[1].data)
        sleep.assert_called_once_with(6.0)


class LoginRetryScopeTests(unittest.TestCase):
    def test_browser_never_reposts_denial_or_offline_response(self):
        for status in (401, 403, 503):
            browser = mock.Mock()
            browser.request.side_effect = [(200, {}, None, b'<input name="password">'),
                                           (status, {}, None, b"authoritative")]
            with mock.patch.object(rate_limit_retry.time, "sleep") as sleep:
                self.assertEqual(gl.core_sign_in(browser, "http://core/node-login?node_id=n", "alice", "pw"),
                                 (status, None, None))
            self.assertEqual(browser.request.call_count, 2)
            sleep.assert_not_called()

    def test_non_login_route_429_is_not_retried(self):
        module = runpy.run_path(str(Path(__file__).with_name("check-core-to-node-transfer-stack.py")))
        response = mock.MagicMock()
        response.__enter__.return_value = response
        response.status, response.headers = 429, {"Retry-After": "6"}
        response.read.return_value = b"unchanged"
        with mock.patch.object(urllib.request, "urlopen", return_value=response) as open_request:
            self.assertEqual(module["request"]("http://core/other", "POST", {})[0], 429)
        open_request.assert_called_once()


if __name__ == "__main__":
    unittest.main()
