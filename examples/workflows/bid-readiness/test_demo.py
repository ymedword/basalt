"""演示控制侧回归：无真实网络或凭据。Offline controller regressions."""
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import demo


class DemoTests(unittest.TestCase):
    def test_unknown_matrix_publication_cannot_dispatch_twice(self):
        from matrix_definition import LABEL
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "source-manifest.json").write_bytes(b"{}")
            agents = {name: {"name": name, "version": 3} for name in ("bid-delivery", "bid-reviewer")}
            demo.save(root / "us19-matrix-agent-config.json", {"source_manifest_sha256": hashlib.sha256(b"{}").hexdigest(), "agents": {name: {"state": "confirmed", "agent": pin} for name, pin in agents.items()}})

            class FakeAPI:
                writes = 0

                def call(self, method, path, body=None):
                    if method == "GET":
                        return {"items": [{"name": name, "eligible": True, "agent": pin} for name, pin in agents.items()]}, {}
                    if path.endswith("validate"):
                        return {"valid": True}, {}
                    self.writes += 1
                    pending = root / "us19-business-bundles" / (LABEL + "-publication-pending.json")
                    self.assert_pending = json.loads(pending.read_text(encoding="utf-8"))["request"] == body
                    raise TimeoutError("unknown publication response")

            api = FakeAPI()
            with patch.object(demo, "LOOP", root), patch.object(demo, "ROOT", root), patch.object(demo, "approved_sources"), patch.object(demo, "build_definitions", return_value={"long": {"name": "original"}}), patch("matrix_definition.build", return_value={"name": "corrected"}):
                with self.assertRaises(TimeoutError):
                    demo.publish(api, True)
                with self.assertRaisesRegex(RuntimeError, "read-only reconciliation"):
                    demo.publish(api, True)
            self.assertTrue(api.assert_pending)
            self.assertEqual(api.writes, 1)

    def test_old_source_revision_refuses_config_and_publication_without_writes(self):
        from unittest.mock import Mock
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "source-manifest.json").write_bytes(b"new source revision")
            demo.save(root / "us19-matrix-agent-config.json", {"source_manifest_sha256": "old", "agents": {}})
            api = Mock()
            api.call.return_value = ({"items": []}, {})
            with patch.object(demo, "LOOP", root), patch.object(demo, "ROOT", root), patch.object(demo, "approved_sources"), patch.object(demo, "build_definitions", return_value={}):
                for operation in (demo.prepare_matrix_agents, lambda value: demo.publish(value, True)):
                    with self.assertRaisesRegex(RuntimeError, "source revision changed"):
                        operation(api)
            self.assertTrue(all(call.args[0] == "GET" for call in api.call.call_args_list))

    def test_start_pins_recorded_version_and_retains_mismatched_receipt(self):
        from matrix_definition import LABEL
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "source-manifest.json").write_bytes(b"{}")
            demo.save(root / "evaluation-protocol.json", {})
            publication = {"definition_uid": "original-definition", "version": 2}
            demo.save(root / "us19-business-published.json", {LABEL: publication})

            class FakeAPI:
                writes = 0

                def call(self, method, path, body, headers):
                    self.writes += 1
                    self.body = body
                    return {"task_id": "original-task", "status": "accepted", "definition_uid": "original-definition", "definition_version": 3}, {}

            api = FakeAPI()
            with patch.object(demo, "LOOP", root), patch.object(demo, "ROOT", root), patch.object(demo, "approved_sources"):
                with self.assertRaisesRegex(RuntimeError, "differs from pinned publication"):
                    demo.run(api, LABEL)
                with self.assertRaisesRegex(RuntimeError, "already recorded"):
                    demo.run(api, LABEL)
            self.assertEqual(api.body["definition_version"], 2)
            self.assertEqual(api.writes, 1)
            stored = json.loads((root / "us19-business-runs.json").read_text(encoding="utf-8"))[LABEL]
            self.assertEqual(stored["task_id"], "original-task")
            self.assertEqual(stored["acceptance_receipt"]["definition_version"], 3)

    def test_limit_reached_keeps_original_run_result(self):
        result = {"status": "limit_reached", "run_id": "original-run", "final_text": "partial"}
        raw = json.dumps({"code": "LIMIT_REACHED", "data": result}).encode()

        class Response(io.BytesIO):
            headers = {}

        api = object.__new__(demo.API)
        api.base = "http://127.0.0.1:1"
        from unittest.mock import Mock
        api.opener = Mock()
        api.opener.open.return_value = Response(raw)
        returned, _ = api.call("POST", "/api/v1/agents/bid-general/invoke", {}, allowed_codes=("OK", "LIMIT_REACHED"))
        self.assertEqual(returned, result)
        api.opener.open.assert_called_once()

    def test_authorized_rerun_keeps_matrix_pin_and_failed_receipt(self):
        from matrix_definition import LABEL
        from matrix_report import FORMAT
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "source-manifest.json").write_bytes(b"{}")
            demo.save(root / "evaluation-protocol.json", {})
            publication = {"definition_uid": "original-matrix", "version": 1}
            failed = {"state": "failed", "task_id": "original-failed-task"}
            demo.save(root / "us19-business-published.json", {LABEL: publication, "long": {"definition_uid": "legacy", "version": 3}})
            demo.save(root / "us19-business-runs.json", {LABEL: failed})

            class FakeAPI:
                writes = 0

                def call(self, method, path, body=None, headers=None):
                    if method == "POST":
                        self.writes += 1
                        self.body, self.headers = body, headers
                        return {"task_id": "new-task", "status": "accepted", "definition_uid": body["definition_uid"], "definition_version": body["definition_version"]}, {}
                    return ({"items": []} if path.endswith("artifacts") else {"status": "succeeded"}), {}

            api = FakeAPI()
            with patch.object(demo, "LOOP", root), patch.object(demo, "ROOT", root), patch.object(demo, "approved_sources"):
                demo.run(api, "long-fixed-v2")
                with self.assertRaisesRegex(RuntimeError, "already recorded"):
                    demo.run(api, "long-fixed-v2")
            stored = json.loads((root / "us19-business-runs.json").read_text(encoding="utf-8"))
            self.assertEqual(stored[LABEL], failed)
            self.assertEqual(stored["long-fixed-v2"]["report_format"], FORMAT)
            self.assertEqual(api.body["definition_uid"], "original-matrix")
            self.assertEqual(api.body["definition_version"], 1)
            self.assertEqual(api.headers["Idempotency-Key"], "us19-bid-run-long-fixed-v2")
            self.assertEqual(api.writes, 1)

    def test_ingestion_unknown_or_failed_is_never_replayed(self):
        for outcome in ["timeout", "failed"]:
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                (root / "roles.json").write_text("{}", encoding="utf-8")
                (root / "source-manifest.json").write_text("{}", encoding="utf-8")
                (root / "sources").mkdir()
                (root / "sources/request-01.md").write_text("synthetic", encoding="utf-8")
                receipt = {"collections": {name: name for name in ["bid-request", "bid-product", "bid-commercial"]}, "memories": {}, "documents": {}, "source_manifest_sha256": hashlib.sha256(b"{}").hexdigest()}
                demo.save(root / "us19-business-setup.json", receipt)

                class FakeAPI:
                    posts = 0

                    def call(self, method, path, *args):
                        if path == "/api/v1/admin/flow-agents":
                            return {"items": []}, {}
                        if path.endswith("uploads"):
                            return {"path": "uploads/request-01.md"}, {}
                        if path.endswith("/documents"):
                            self.posts += 1
                            stored = json.loads((root / "us19-business-setup.json").read_text(encoding="utf-8"))
                            assert stored["documents"]["request-01.md"]["dispatch"]["state"] == "pending"
                            if outcome == "timeout":
                                raise TimeoutError("response unavailable after dispatch")
                            return {"status": "failed"}, {}
                        raise AssertionError(path)

                api = FakeAPI()
                manifest = {"files": [{"path": "sources/request-01.md", "sha256": "fixture"}]}
                with patch.object(demo, "ROOT", root), patch.object(demo, "LOOP", root), patch.object(demo, "approved_sources", return_value=manifest):
                    with self.assertRaises((TimeoutError, RuntimeError)):
                        demo.setup(api, True)
                    with self.assertRaisesRegex(RuntimeError, "reconciliation"):
                        demo.setup(api, True)
                self.assertEqual(api.posts, 1)

    def test_controller_context_is_hash_bound(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "scenario.py"
            source.write_bytes(b"approved scenario")
            manifest = {"files": [], "context_files": [{"path": "scenario.py", "bytes": len(source.read_bytes()), "sha256": hashlib.sha256(source.read_bytes()).hexdigest()}]}
            demo.save(root / "source-manifest.json", manifest)
            with patch.object(demo, "ROOT", root):
                demo.approved_sources()
                source.write_bytes(b"changed scenario!")
                with self.assertRaisesRegex(RuntimeError, "source changed"):
                    demo.approved_sources()


if __name__ == "__main__":
    unittest.main()
