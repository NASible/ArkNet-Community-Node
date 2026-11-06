"""Offline regression checks for the actual nested harness HTTP functions."""
import ast
import json
from pathlib import Path
import unittest
import urllib.request
import urllib.error
from unittest import mock

from scripts.enrollment_signing import EnrollmentSigner


ROOT = Path(__file__).resolve().parent
ENDPOINT = "/api/federation/v1/enrollments"


class HarnessEnrollmentTests(unittest.TestCase):
    def test_admin_harness_enrollment_uses_proof_not_admin_token(self):
        source = ast.parse((ROOT / "check-core-admin-stack.py").read_text())
        function = next(n for n in ast.walk(source) if isinstance(n, ast.FunctionDef) and n.name == "request")
        captured = []

        class Response:
            code = 200
            headers = mock.Mock()
            headers.get_content_type.return_value = "application/json"

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                pass

            def read(self):
                return b'{"node_id":"node_a","enrollment_status":"PENDING"}'

        def open_request(req, timeout):
            captured.append(req)
            return Response()

        with EnrollmentSigner(interval=0) as signer, mock.patch.object(urllib.request, "urlopen", side_effect=open_request):
            namespace = dict(base="http://localhost", tokens={"decide": "never-send-me"},
                             enrollment=ENDPOINT, signer=signer, json=json, urllib=urllib)
            exec(compile(ast.Module(body=[function], type_ignores=[]), "harness", "exec"), namespace)
            receipt = namespace["request"]("POST", ENDPOINT,
                                           {"node_id": "node_a", "version": "v1", "capabilities": {}}, credential=None)
        self.assertEqual(receipt, {"node_id": "node_a", "enrollment_status": "PENDING"})
        self.assertEqual(len(captured), 1)
        request = captured[0]
        self.assertIsNone(request.get_header("Authorization"))
        self.assertIsNotNone(request.get_header("X-arknet-node-signature"))
        self.assertEqual(set(json.loads(request.data)), {"node_id", "version", "capabilities", "public_key"})

    def test_enrollment_status_always_sends_operator_session(self):
        source = ast.parse((ROOT / "check-enrollment-stack.py").read_text())
        function = next(n for n in ast.walk(source) if isinstance(n, ast.FunctionDef) and n.name == "fetch_status")
        operator = mock.Mock(headers={"Cookie": "__Host-arknet_operator=offline-session"})
        captured = []

        def open_request(req, timeout):
            captured.append(req)
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.read.return_value = b'{"state":"ENROLLMENT_PENDING"}'
            return response

        namespace = dict(env={"ARKNET_AGENT_PORT": "12345"}, operator=operator, json=json, urllib=urllib)
        exec(compile(ast.Module(body=[function], type_ignores=[]), "harness", "exec"), namespace)
        with mock.patch.object(urllib.request, "urlopen", side_effect=open_request):
            # The same session must be sent initially and after restart/recreation.
            for _ in range(3):
                self.assertEqual(namespace["fetch_status"](), {"state": "ENROLLMENT_PENDING"})
        for req in captured:
            headers = dict(req.header_items()) if isinstance(req, urllib.request.Request) else {}
            self.assertEqual(headers.get("Cookie"), operator.headers["Cookie"],
                             "protected Node status requires the supported operator session")

    def test_enrollment_harness_signs_retries_and_forbidden_metadata(self):
        source = ast.parse((ROOT / "check-enrollment-stack.py").read_text())
        function = next(n for n in ast.walk(source) if isinstance(n, ast.FunctionDef) and n.name == "post")
        captured = []

        def open_request(req, timeout):
            captured.append(req)
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.status = 200
            response.read.return_value = b'{"node_id":"node_a","enrollment_status":"PENDING"}'
            return response

        with EnrollmentSigner(interval=0) as signer, mock.patch.object(urllib.request, "urlopen", side_effect=open_request):
            namespace = dict(endpoint="http://localhost" + ENDPOINT, node_id="node_a", signer=signer,
                             json=json, urllib=urllib)
            exec(compile(ast.Module(body=[function], type_ignores=[]), "harness", "exec"), namespace)
            for extra in ({}, {}, {"wan_ip": "must-not-be-stored"}):
                namespace["post"](dict(node_id="node_a", version="v1", capabilities={}, **extra))
        public_keys = []
        for req in captured:
            self.assertIsNotNone(req.get_header("X-arknet-node-signature"))
            public_keys.append(json.loads(req.data)["public_key"])
        self.assertEqual(len(set(public_keys)), 1)
        self.assertEqual(json.loads(captured[-1].data)["wan_ip"], "must-not-be-stored")


if __name__ == "__main__":
    unittest.main()
