"""矩阵缺失、重复、错来源不可被计为完整覆盖。Offline coverage regressions."""
import json
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, Mock

import collect
from collect import coverage, original_text


def fixture():
    return {f"review_{group}": json.dumps({"report": {"result": "\n".join(
        f"R{i:02d}|原要求|待验证|request-{(i - 1) // 6 + 1:02d}.md#R{i:02d}|技术负责人|补测试报告"
        for i in range((group - 1) * 12 + 1, group * 12 + 1)), "sources": "原始要求", "open_questions": "待补证"}})
        for group in range(1, 5)}


class CollectionTests(unittest.TestCase):
    def test_original_invoke_receipt_is_bound_to_run(self):
        record = {"result": {"run_id": "run-a", "final_text": "original synchronous response"}}
        self.assertEqual(original_text(record, {"run_id": "run-a", "final_text": ""}),
                         ("original synchronous response", "invoke_receipt"))
        self.assertEqual(original_text({}, {"run_id": "run-b", "final_text": "flow output"}),
                         ("flow output", "run_detail"))
        with self.assertRaises(ValueError):
            original_text(record, {"run_id": "different", "final_text": ""})

    def test_complete_structure_is_not_business_verdict(self):
        result = coverage(fixture())
        self.assertTrue(result["structure_complete"])
        self.assertIn("not established", result["business_correctness"])

    def test_missing_duplicate_or_wrong_source_rejected(self):
        for changed in ["missing", "duplicate", "source", "columns", "status"]:
            with self.subTest(changed=changed):
                items = fixture()
                if changed == "missing":
                    del items["review_4"]
                else:
                    report = json.loads(items["review_1"])
                    old = report["report"]["result"]
                    replacement = {"duplicate": ("R02|", "R01|"), "source": ("request-01.md#R01", "request-08.md#R01"),
                                   "columns": ("|技术负责人|", "||"), "status": ("|待验证|", "|待补证|")}[changed]
                    report["report"]["result"] = old.replace(*replacement, 1)
                    items["review_1"] = json.dumps(report)
                self.assertFalse(coverage(items)["structure_complete"])

    def test_missing_or_invalid_handoff_fields_rejected(self):
        for field in ["sources", "open_questions"]:
            for replacement in [None, 42]:
                items = fixture()
                report = json.loads(items["review_1"])
                if replacement is None:
                    del report["report"][field]
                else:
                    report["report"][field] = replacement
                items["review_1"] = json.dumps(report)
                self.assertFalse(coverage(items)["structure_complete"])
        items["review_1"] = "invalid JSON"
        self.assertFalse(coverage(items)["structure_complete"])

    def test_collector_uses_recorded_matrix_format_and_keeps_raw_runs(self):
        from test_matrix import fixture as matrix_fixture
        import matrix_report
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            digest = hashlib.sha256(b"{}").hexdigest()
            (root / "source-manifest.json").write_bytes(b"{}")
            record = {"task_id": "original-task", "report_format": matrix_report.FORMAT,
                      "source_manifest_sha256": digest, "state": "succeeded"}
            (root / "us19-business-runs.json").write_text(json.dumps({"long-fixed-v1": record}))
            raw = {f"run-{group}": json.dumps(matrix_fixture(group), ensure_ascii=False) for group in range(1, 5)}
            task = {"status": "succeeded", "attempts": [{"node_id": f"review_{g}", "run_id": f"run-{g}", "state": "succeeded"} for g in range(1, 5)]}

            class ReadOnlyAPI:
                def call(self, method, path):
                    if method != "GET":
                        raise AssertionError("collector must not mutate")
                    if path.startswith("/api/v1/flow-tasks/"):
                        return task, {}
                    run_id = path.rsplit("/", 1)[1]
                    return {"run_id": run_id, "status": "completed", "final_text": raw[run_id]}, {}

            with patch.object(collect, "LOOP", root), patch.object(collect, "ROOT", root), patch.object(collect, "approved_sources"), patch("builtins.print"):
                collect.collect(ReadOnlyAPI(), "long-fixed-v1")
                target = root / "us19-business-output/long-fixed-v1"
                result = json.loads((target / "collection.json").read_text(encoding="utf-8"))
                self.assertTrue(result["coverage"]["structure_complete"])
                self.assertIn("not established", result["coverage"]["business_correctness"])
                for group in range(1, 5):
                    original = raw[f"run-{group}"]
                    self.assertEqual((target / f"review_{group}.txt").read_text(encoding="utf-8"), original)
                    self.assertEqual(result["original_runs"][f"review_{group}"]["sha256"], hashlib.sha256(original.encode()).hexdigest())
                self.assertIn("原文与来源列", (target / "report.md").read_text(encoding="utf-8"))
                (root / "source-manifest.json").write_bytes(b'{"revision":2}')
                api = Mock()
                with self.assertRaisesRegex(ValueError, "source revision differs"):
                    collect.collect(api, "long-fixed-v1")
                api.call.assert_not_called()


if __name__ == "__main__":
    unittest.main()
