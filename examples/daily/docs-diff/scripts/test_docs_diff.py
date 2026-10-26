#!/usr/bin/env python3
"""D4 `docs-diff` 的业务脚本单测（颗粒度文档 §4.4「D4 只读 MCP」的落点）。

运行 / Run：

    cd examples/daily/docs-diff/scripts
    py -3 -B -m unittest -v

覆盖：首跑只记基线不编造变更、无变化仍出报告、新增 / 修改 / 删除三集合、内容哈希
相等 / 不等（含同字节数的内容变化）、四类计数不变量、空根、两份快照不同源的五种拒收、
基线版本与形态拒收、库根校验（工作区文件 / 超过演示上限）、CLI 失败零写入、
报告路径无关（可复现）。

单测直接读 `../fixtures/docs/a.md` / `b.md` 的**真实字节**：夹具与断言不得各自漂移。
The tests hash the shipped fixture bytes instead of copying them inline, and `-B` keeps
`__pycache__` out of the tree (the release clean-checkout check requires a clean worktree).
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import docs_diff
from docs_diff import ScriptError

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures" / "docs"
NOW = "2026-10-25T09:15:00+08:00"
MODIFIED = "2026-10-24T00:00:00Z"
BASELINE_AT = "2026-10-24T09:15:00+08:00"


def fixture(name: str) -> bytes:
    return (FIXTURES / name).read_bytes()


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def entry(path: str, data: bytes, *, modified_at: str = MODIFIED) -> dict:
    return {"path": path, "size": len(data), "modified_at": modified_at}


def meta_entry(path: str, data: bytes, *, modified_at: str = MODIFIED) -> dict:
    return {"path": path, "size": len(data), "modified_at": modified_at, "sha256": digest(data)}


def snapshot(pairs: list[tuple[str, bytes]]) -> tuple[list[dict], dict]:
    return ([entry(path, data) for path, data in pairs],
            {"documents": [meta_entry(path, data) for path, data in pairs]})


def baseline_of(pairs: list[tuple[str, bytes]], *, observed_at: str = BASELINE_AT) -> dict:
    return {
        "schema_version": 1,
        "agent": "docs-diff",
        "observed_at": observed_at,
        "documents": {path: {"sha256": digest(data), "size": len(data)} for path, data in pairs},
    }


def run(pairs: list[tuple[str, bytes]], baseline: dict | None = None, *, now: str = NOW) -> dict:
    documents, metadata = snapshot(pairs)
    return docs_diff.diff_documents(documents, metadata, baseline, now=now)


def run_cli(documents: list[dict], metadata: dict, *, root: Path, output: Path, state: Path,
            indent: int | None = None) -> tuple[int, str, str]:
    docs_path = root / "documents.json"
    meta_path = root / "metadata.json"
    docs_path.write_text(json.dumps(documents, ensure_ascii=False, indent=indent), encoding="utf-8")
    meta_path.write_text(json.dumps(metadata, ensure_ascii=False, indent=indent), encoding="utf-8")
    out, err = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = docs_diff.main([
            "--documents", str(docs_path),
            "--metadata", str(meta_path),
            "--state", str(state),
            "--output", str(output),
            "--now", NOW,
        ])
    return code, out.getvalue(), err.getvalue()


class DocsDiffTest(unittest.TestCase):
    def test_first_run_records_baseline_without_fabricating_changes(self):
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        result = run(pairs)
        report, state = result["report"], result["state"]
        self.assertEqual(report["mode"], "baseline_created")
        self.assertTrue(report["baseline_missing"])
        self.assertIsNone(report["baseline_observed_at"])
        # 逆改点：首跑就把被记录的文档写成 added → 集合非空 / 出现了 changes_detected。
        self.assertNotIn("changes_detected", report)
        self.assertEqual(report["sets"], {"added": [], "modified": [], "deleted": [], "unchanged": []})
        self.assertEqual(report["counts"]["current"], 2)
        self.assertEqual([record["status"] for record in report["documents"]], ["baseline", "baseline"])
        self.assertIn("无历史基线", report["message"])
        self.assertNotIn("本次无变化", report["message"])
        self.assertEqual(state["documents"]["a.md"]["sha256"], digest(fixture("a.md")))

    def test_state_keeps_only_content_hash_and_size(self):
        state = run([("a.md", fixture("a.md"))])["state"]
        self.assertEqual(set(state["documents"]["a.md"]), {"sha256", "size"})
        self.assertNotIn("modified_at", json.dumps(state))

    def test_report_carries_no_input_paths_or_file_hashes(self):
        # 报告只认内容：输入文件路径 / 文件哈希 / mtime 都不进报告，否则换台机器或换个
        # JSON 缩进就会得到不同的报告哈希，"同一份内容 → 同一份报告"不再成立。
        report = run([("a.md", fixture("a.md"))])["report"]
        serialized = json.dumps(report, ensure_ascii=False)
        self.assertNotIn("inputs", report)
        self.assertNotIn("modified_at", serialized)
        self.assertNotIn("documents.json", serialized)
        self.assertNotIn("metadata.json", serialized)

    def test_unchanged_run_reports_no_changes(self):
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        report = run(pairs, baseline_of(pairs))["report"]
        self.assertEqual(report["mode"], "compared")
        self.assertFalse(report["changes_detected"])
        self.assertEqual(report["sets"]["unchanged"], ["a.md", "b.md"])
        self.assertEqual(report["sets"]["added"], [])
        self.assertIn("本次无变化", report["message"])
        for record in report["documents"]:
            # 逆改点：拿 mtime / size 当判据，或把未变写成修改 → 这两个值会不等。
            self.assertEqual(record["status"], "unchanged")
            self.assertEqual(record["sha256"], record["previous_sha256"])

    def test_modified_document_has_different_hash(self):
        old_a = fixture("a.md") + "\n<!-- 旧版本 -->\n".encode("utf-8")
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        report = run(pairs, baseline_of([("a.md", old_a), ("b.md", fixture("b.md"))]))["report"]
        self.assertTrue(report["changes_detected"])
        self.assertEqual(report["sets"]["modified"], ["a.md"])
        self.assertEqual(report["sets"]["unchanged"], ["b.md"])
        records = {record["path"]: record for record in report["documents"]}
        self.assertNotEqual(records["a.md"]["sha256"], records["a.md"]["previous_sha256"])
        self.assertEqual(records["a.md"]["sha256"], digest(fixture("a.md")))
        self.assertEqual(records["b.md"]["sha256"], records["b.md"]["previous_sha256"])

    def test_same_size_change_is_still_modified(self):
        # 逆改点：拿 size 当变更判据 → 只改一个空白（字节数不变）会被漏判成未变。
        data = fixture("a.md")
        changed = data[:-1] + b" "  # 末尾换行换成空格：同字节数、不同内容
        self.assertEqual(len(changed), len(data))
        report = run([("a.md", changed)], baseline_of([("a.md", data)]))["report"]
        self.assertTrue(report["changes_detected"])
        self.assertEqual(report["sets"]["modified"], ["a.md"])
        record = report["documents"][0]
        self.assertEqual(record["size"], record["previous_size"])
        self.assertNotEqual(record["sha256"], record["previous_sha256"])

    def test_added_and_deleted_sets(self):
        gone = "# 已删文档\n\n这份文档不在当前根里。\n".encode("utf-8")
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        report = run(pairs, baseline_of([("a.md", fixture("a.md")), ("old/gone.md", gone)]))["report"]
        self.assertTrue(report["changes_detected"])
        self.assertEqual(report["sets"]["added"], ["b.md"])
        self.assertEqual(report["sets"]["deleted"], ["old/gone.md"])
        self.assertEqual(report["sets"]["unchanged"], ["a.md"])
        records = {record["path"]: record for record in report["documents"]}
        self.assertIsNone(records["b.md"]["previous_sha256"])
        self.assertIsNone(records["old/gone.md"]["sha256"])
        self.assertEqual(records["old/gone.md"]["previous_sha256"], digest(gone))

    def test_counts_are_consistent_with_sets(self):
        gone = b"gone\n"
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        report = run(pairs, baseline_of([("a.md", b"old a\n"), ("old/gone.md", gone)]))["report"]
        counts = report["counts"]
        self.assertEqual(counts["added"] + counts["modified"] + counts["unchanged"], counts["current"])
        self.assertEqual(counts["deleted"] + counts["modified"] + counts["unchanged"], counts["baseline"])
        self.assertEqual(sum(counts[key] for key in ("added", "modified", "deleted", "unchanged")),
                         len(report["documents"]))

    def test_empty_root_first_run(self):
        report = run([])["report"]
        self.assertEqual(report["mode"], "baseline_created")
        self.assertEqual(report["counts"]["current"], 0)
        self.assertEqual(report["documents"], [])
        self.assertIn("无历史基线", report["message"])

    def test_empty_root_after_baseline_reports_deletions(self):
        pairs = [("a.md", fixture("a.md")), ("b.md", fixture("b.md"))]
        report = run([], baseline_of(pairs))["report"]
        self.assertEqual(report["mode"], "compared")
        self.assertTrue(report["changes_detected"])
        self.assertEqual(report["sets"]["deleted"], ["a.md", "b.md"])
        self.assertIn("删除 2", report["message"])

    def test_workspace_marker_in_listing_is_refused(self):
        # 逆改点：docs-readonly 没传 -root → 清单里出现工作区文件，仍逐份取元信息。
        pairs = [("a.md", fixture("a.md")), ("AGENTS.md", b"# workspace\n")]
        documents, metadata = snapshot(pairs)
        with self.assertRaisesRegex(ScriptError, "只读库根疑似指向整个工作区"):
            docs_diff.diff_documents(documents, metadata, None, now=NOW)

    def test_oversized_listing_is_refused_before_any_metadata(self):
        pairs = [(f"doc-{index:02d}.md", b"x") for index in range(docs_diff.MAX_DOCUMENTS + 1)]
        documents, metadata = snapshot(pairs)
        with self.assertRaisesRegex(ScriptError, "超过演示上限"):
            docs_diff.diff_documents(documents, metadata, None, now=NOW)

    def test_demo_sized_library_is_accepted(self):
        pairs = [(f"doc-{index:02d}.md", b"x") for index in range(docs_diff.MAX_DOCUMENTS)]
        result = run(pairs)
        self.assertEqual(result["report"]["mode"], "baseline_created")

    def test_metadata_missing_entry_is_refused(self):
        documents = [entry("a.md", fixture("a.md")), entry("b.md", fixture("b.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md"))]}
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("metadata 缺少", str(caught.exception))
        self.assertIn("b.md", str(caught.exception))

    def test_metadata_extra_entry_is_refused(self):
        documents = [entry("a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md")), meta_entry("c.md", b"c\n")]}
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("多出清单外", str(caught.exception))
        self.assertIn("c.md", str(caught.exception))

    def test_size_mismatch_is_refused(self):
        documents = [entry("a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md"))]}
        metadata["documents"][0]["size"] += 1
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("size", str(caught.exception))
        self.assertIn("a.md", str(caught.exception))

    def test_modified_at_mismatch_is_refused(self):
        documents = [entry("a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md"), modified_at="2026-10-23T00:00:00Z")]}
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("modified_at", str(caught.exception))

    def test_invalid_sha_is_refused(self):
        documents = [entry("a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md"))]}
        metadata["documents"][0]["sha256"] = "ABC"
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("sha256", str(caught.exception))

    def test_duplicate_path_is_refused(self):
        documents = [entry("a.md", fixture("a.md")), entry("a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("a.md", fixture("a.md"))]}
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("重复路径", str(caught.exception))

    def test_escape_path_is_refused(self):
        documents = [entry("../a.md", fixture("a.md"))]
        metadata = {"documents": [meta_entry("../a.md", fixture("a.md"))]}
        with self.assertRaises(ScriptError) as caught:
            docs_diff.diff_documents(documents, metadata, None, now=NOW)
        self.assertIn("path", str(caught.exception))

    def test_baseline_schema_version_mismatch_is_refused(self):
        baseline = baseline_of([("a.md", fixture("a.md"))])
        baseline["schema_version"] = 2
        with self.assertRaises(ScriptError) as caught:
            docs_diff.validate_baseline(baseline)
        self.assertIn("schema_version", str(caught.exception))

    def test_baseline_without_observed_at_is_refused(self):
        baseline = baseline_of([("a.md", fixture("a.md"))])
        del baseline["observed_at"]
        with self.assertRaises(ScriptError) as caught:
            docs_diff.validate_baseline(baseline)
        self.assertIn("observed_at", str(caught.exception))

    def test_cli_first_and_second_run_write_report_and_state(self):
        documents, metadata = snapshot([("a.md", fixture("a.md")), ("b.md", fixture("b.md"))])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = root / "state" / "baseline.json"
            first_out = root / "runs" / "first" / "changes.json"
            code, out, err = run_cli(documents, metadata, root=root, output=first_out, state=state)
            self.assertEqual((code, err), (0, ""))
            first = json.loads(out)
            self.assertEqual(first["mode"], "baseline_created")
            self.assertIsNone(first["changes_detected"])
            self.assertTrue(state.exists())
            self.assertTrue(first_out.exists())
            state_bytes = state.read_bytes()

            second_out = root / "runs" / "second" / "changes.json"
            code, out, err = run_cli(documents, metadata, root=root, output=second_out, state=state)
            self.assertEqual((code, err), (0, ""))
            second = json.loads(out)
            self.assertEqual(second["mode"], "compared")
            self.assertFalse(second["changes_detected"])
            self.assertTrue(second_out.exists())  # 无变化也出报告
            # 同内容 + 同 --now：基线字节不变（没有无谓的漂移）。
            self.assertEqual(state.read_bytes(), state_bytes)

    def test_cli_failure_writes_nothing(self):
        documents, _ = snapshot([("a.md", fixture("a.md"))])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            output = root / "runs" / "x" / "changes.json"
            state = root / "state" / "baseline.json"
            code, _, err = run_cli(documents, {"documents": []}, root=root, output=output, state=state)
            self.assertEqual(code, 1)
            self.assertIn("metadata 缺少", err)
            self.assertFalse(output.exists())
            self.assertFalse(output.parent.exists())  # 父目录也不建
            self.assertFalse(state.exists())

    def test_cli_report_is_path_and_formatting_independent(self):
        documents, metadata = snapshot([("a.md", fixture("a.md")), ("b.md", fixture("b.md"))])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            first = root / "a" / "changes.json"
            second = root / "b" / "changes.json"
            run_cli(documents, metadata, root=root, output=first, state=root / "state-a" / "baseline.json")
            # 第二份输入的缩进不同 → 文件哈希不同，但报告只认解析后的内容。
            run_cli(documents, metadata, root=root, output=second,
                    state=root / "state-b" / "baseline.json", indent=2)
            self.assertEqual(first.read_bytes(), second.read_bytes())


if __name__ == "__main__":
    unittest.main()
