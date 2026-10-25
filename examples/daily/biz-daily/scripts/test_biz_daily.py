#!/usr/bin/env python3
"""D3 `biz-daily` 的业务脚本单测（颗粒度文档 §4.4「D3 数据质量三类」的落点）。

运行 / Run：

    cd examples/daily/biz-daily/scripts
    py -3 -m unittest -v

覆盖：夹具的三类异常逐类计数、四类互斥不变量、坏行不进指标、重复以首次出现为准、
非法值的原因码、截断标记、空输入与全坏输入、CLI 写序与失败不落盘。

The first test reads the shipped fixture (../fixtures/rows.json) instead of an inline
copy: the fixture and the assertions must not drift apart.
"""

from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from decimal import Decimal
from pathlib import Path

import biz_daily
from biz_daily import ScriptError

NOW = "2026-10-25T09:00:00+08:00"
FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "rows.json"


def load_fixture() -> list[dict]:
    return json.loads(FIXTURE.read_text(encoding="utf-8"))["rows"]


def run(payload=None, *, now=NOW, max_examples=5) -> dict:
    """测试入口：缺省喂夹具，固定运行时钟。"""
    return biz_daily.daily(load_fixture() if payload is None else payload, now=now, max_examples=max_examples)


def row(order_id="SO-1", order_date="2026-10-01", region="华东", amount=100.0, status="paid", **extra):
    data = {"order_id": order_id, "order_date": order_date, "region": region, "amount": amount, "status": status}
    data.update(extra)
    return data


class BizDailyTest(unittest.TestCase):
    def test_fixture_counts_three_anomaly_classes(self):
        out = run()
        # 夹具里刻意埋了三类坏数据：缺失 3 行（region=null / amount 缺键 / status 空串）、
        # 重复 2 行（SO-1003 / SO-1002）、非法 4 行（金额文本 / 负数 / 状态枚举外 / 日期非法）。
        self.assertEqual(out["quality"]["counts"], {"valid": 15, "missing": 3, "duplicate": 2, "invalid": 4})
        self.assertEqual(out["quality"]["total"], 24)

    def test_classes_are_mutually_exclusive(self):
        counts = run()["quality"]["counts"]
        self.assertEqual(sum(counts.values()), 24)  # 逆改点：一行数两遍 → 和 > 总行数

    def test_missing_fields_are_not_imputed(self):
        # 缺金额的行不得按 0 计入：若按 0 计，有效行会变成 2、毛额仍是 100。
        out = run([row(order_id="A", amount=None), row(order_id="B", amount=100.0)])
        self.assertEqual(out["quality"]["counts"]["missing"], 1)
        self.assertEqual(out["metrics"]["valid_orders"], 1)  # 逆改点：缺失补 0 当有效行 → 红
        self.assertEqual(out["metrics"]["gross_amount"], 100.0)
        self.assertEqual(out["quality"]["missing"]["by_field"], {"amount": 1})

    def test_missing_region_does_not_create_an_empty_bucket(self):
        out = run()
        regions = [entry["region"] for entry in out["metrics"]["by_region"]]
        self.assertNotIn("", regions)  # 逆改点：缺大区当空串分组 → 红
        self.assertEqual(regions, ["华东", "华北", "华南", "西南"])

    def test_duplicate_keeps_first_occurrence(self):
        out = run()
        # SO-1003 首次 2450、副本 2600：只有首次进指标，副本整行剔除。
        self.assertEqual(out["quality"]["duplicate"]["order_ids"], ["SO-1002", "SO-1003"])
        self.assertEqual(out["metrics"]["gross_amount"], 11890.5)  # 逆改点：用副本覆盖首次 → 红
        firsts = {entry["order_id"]: entry["first_row"] for entry in out["quality"]["duplicate"]["examples"]}
        self.assertEqual(firsts, {"SO-1003": 3, "SO-1002": 2})

    def test_duplicate_precedes_invalid(self):
        # 同一行既是重复又有坏金额：按优先级记重复，不再记非法（行级计数互斥）。
        source = [row(order_id="A", amount=100.0), row(order_id="A", amount="N/A")]
        counts = run(source)["quality"]["counts"]
        self.assertEqual(counts, {"valid": 1, "missing": 0, "duplicate": 1, "invalid": 0})

    def test_invalid_amount_reasons(self):
        out = run([row(order_id="A", amount="N/A"), row(order_id="B", amount=-300.0)])
        reasons = out["quality"]["invalid"]["by_reason"]
        self.assertEqual(reasons["amount_not_numeric"], 1)  # 逆改点：文本金额按 0 处理 → 红
        self.assertEqual(reasons["amount_negative"], 1)
        self.assertEqual(out["quality"]["invalid"]["by_field"], {"amount": 2})
        self.assertEqual(out["metrics"]["computed"], False)  # 没有有效行：金额是 null，不是 0.00

    def test_invalid_status_and_date_reasons(self):
        out = run([row(order_id="A", status="cancelled"), row(order_id="B", order_date="2026-13-40")])
        reasons = out["quality"]["invalid"]["by_reason"]
        self.assertEqual(reasons["status_unknown"], 1)  # 逆改点：未知状态猜成 paid → 红
        self.assertEqual(reasons["order_date_unparseable"], 1)

    def test_metrics_recompute_from_valid_rows(self):
        metrics = run()["metrics"]
        self.assertTrue(metrics["computed"])
        self.assertEqual(metrics["valid_orders"], 15)
        self.assertEqual(metrics["status_counts"], {"paid": 9, "refunded": 3, "pending": 3})
        self.assertEqual(Decimal(str(metrics["gross_amount"])), Decimal("11890.50"))
        self.assertEqual(Decimal(str(metrics["refunded_amount"])), Decimal("1190.00"))
        self.assertEqual(Decimal(str(metrics["pending_amount"])), Decimal("2170.00"))
        self.assertEqual(Decimal(str(metrics["net_amount"])), Decimal("10700.50"))
        self.assertEqual(Decimal(str(metrics["average_paid_amount"])), Decimal("1321.17"))
        self.assertEqual(metrics["currency"], "CNY")

    def test_region_sums_match_net_amount(self):
        metrics = run()["metrics"]
        # 恒等式：各分区净额之和 == 总净额；顺序按大区名排序，可逐字比对。
        region_sum = sum(Decimal(str(entry["net_amount"])) for entry in metrics["by_region"])
        self.assertEqual(region_sum, Decimal(str(metrics["net_amount"])))
        self.assertEqual(
            [(entry["region"], entry["orders"]) for entry in metrics["by_region"]],
            [("华东", 6), ("华北", 5), ("华南", 2), ("西南", 2)],
        )

    def test_examples_capped_marks_truncated(self):
        quality = run(max_examples=1)["quality"]
        self.assertTrue(quality["truncated"])  # 逆改点：截断而不标记 → 红
        self.assertEqual(
            quality["truncation"],
            {
                "missing": {"shown": 1, "total": 3},
                "duplicate": {"shown": 1, "total": 2},
                "invalid": {"shown": 1, "total": 4},
            },
        )
        self.assertEqual([len(quality[key]["examples"]) for key in ("missing", "duplicate", "invalid")], [1, 1, 1])
        # 计数不受示例上限影响：截断只影响展示。
        self.assertEqual(quality["counts"], {"valid": 15, "missing": 3, "duplicate": 2, "invalid": 4})

    def test_no_truncation_under_budget(self):
        quality = run(max_examples=5)["quality"]
        self.assertFalse(quality["truncated"])
        self.assertEqual(quality["truncation"], {})

    def test_empty_input_reports_nothing_computed(self):
        out = run([])
        self.assertEqual(out["quality"]["counts"], {"valid": 0, "missing": 0, "duplicate": 0, "invalid": 0})
        self.assertEqual(out["message"], "本次 0 行：输入为空，未计算任何指标")
        self.assertFalse(out["metrics"]["computed"])
        self.assertIsNone(out["metrics"]["net_amount"])  # 逆改点：没数据写 0.00 → 红

    def test_all_rows_bad_reports_no_usable_rows(self):
        out = run([row(order_id="A", amount=None), row(order_id="B", status="???")])
        self.assertIn("中没有可用行", out["message"])
        self.assertFalse(out["metrics"]["computed"])
        self.assertEqual(out["metrics"]["by_region"], [])

    def test_invalid_top_level_shape_raises(self):
        with self.assertRaises(ScriptError):  # 逆改点：静默补默认值 → 红
            run({"bad": "shape"})

    def test_dbhub_envelope_is_unwrapped(self):
        # 真实返回形态（@bytebase/dbhub 1.5.0 实测 2026-10-26）：模型原样落盘整段
        # content 文本，脚本自己解到 statements[0].rows——不让模型挑结构。
        envelope = {
            "success": True,
            "data": {
                "statements": [{"sql": "SELECT ...", "rows": load_fixture(), "count": 24}],
                "source_id": "default",
            },
        }
        out = run(envelope)
        self.assertEqual(out["quality"]["counts"]["valid"], 15)
        self.assertEqual(out["quality"]["counts"]["missing"], 3)

    def test_dbhub_readonly_violation_raises(self):
        # 写操作被 dbhub 以 READONLY_VIOLATION 拒绝时，脚本必须报错退出，
        # 不得把「零行」当成本次结果（逆改点：忽略 success=false → 红）。
        envelope = {
            "success": False,
            "error": "Read-only mode is enabled. Only the following SQL operations are allowed: select, with, explain, pragma",
            "code": "READONLY_VIOLATION",
        }
        with self.assertRaises(ScriptError) as caught:
            run(envelope)
        self.assertIn("READONLY_VIOLATION", str(caught.exception))

    def test_dbhub_multiple_statements_raise(self):
        envelope = {
            "success": True,
            "data": {
                "statements": [{"rows": load_fixture()}, {"rows": []}],
                "source_id": "default",
            },
        }
        with self.assertRaises(ScriptError) as caught:
            run(envelope)
        self.assertIn("一条语句", str(caught.exception))

    def test_non_object_row_raises(self):
        with self.assertRaises(ScriptError):
            run(["not-a-row"])

    def test_naive_clock_rejected_by_cli(self):
        # `--now` 不带时区偏移会让「今天」取决于解释器所在时区；CLI 必须拒收。
        with tempfile.TemporaryDirectory() as raw:
            workdir = Path(raw)
            orders = workdir / "orders.json"
            orders.write_text("[]", encoding="utf-8")
            output = workdir / "runs" / "naive" / "daily.json"
            with contextlib.redirect_stderr(io.StringIO()):
                code = biz_daily.main(["--orders", str(orders), "--output", str(output), "--now", "2026-10-25T09:00:00"])
            self.assertEqual(code, 1)
            self.assertFalse(output.exists())
        with self.assertRaises(ScriptError):
            biz_daily.parse_clock("2026-10-25T09:00:00")

    def test_negative_max_examples_rejected(self):
        with self.assertRaises(ScriptError):
            run(max_examples=-1)

    def test_cli_writes_report_and_leaves_nothing_on_failure(self):
        with tempfile.TemporaryDirectory() as raw:
            workdir = Path(raw)
            orders = workdir / "orders.json"
            orders.write_text(json.dumps({"rows": load_fixture()}, ensure_ascii=False), encoding="utf-8")
            first = workdir / "runs" / "first" / "daily.json"
            second = workdir / "runs" / "second" / "daily.json"

            buffer = io.StringIO()
            with contextlib.redirect_stdout(buffer):
                code = biz_daily.main(["--orders", str(orders), "--output", str(first), "--now", NOW])
            self.assertEqual(code, 0)
            summary = json.loads(buffer.getvalue())
            report = json.loads(first.read_text(encoding="utf-8"))
            self.assertEqual(report["input"]["sha256"], summary["input"]["sha256"])
            self.assertEqual(report["message"], summary["message"])
            self.assertEqual(report["quality"]["counts"]["valid"], 15)

            # 同一输入、同一时钟：两次运行逐字节一致（报告不含路径以外的易变字段）。
            with contextlib.redirect_stdout(io.StringIO()):
                code = biz_daily.main(["--orders", str(orders), "--output", str(second), "--now", NOW])
            self.assertEqual(code, 0)
            self.assertEqual(first.read_bytes(), second.read_bytes())

            # 输入非法：非零退出，且一个字节都不写。
            broken = workdir / "broken.json"
            broken.write_text('{"bad": "shape"}', encoding="utf-8")
            third = workdir / "runs" / "third" / "daily.json"
            with contextlib.redirect_stderr(io.StringIO()):
                code = biz_daily.main(["--orders", str(broken), "--output", str(third), "--now", NOW])
            self.assertEqual(code, 1)
            self.assertFalse(third.exists())
            self.assertFalse(third.parent.exists())


if __name__ == "__main__":
    unittest.main()
