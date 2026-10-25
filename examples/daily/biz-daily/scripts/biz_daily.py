#!/usr/bin/env python3
"""经营日报的数据质量检查与指标计算（颗粒度文档 §4.4「D3 数据质量三类」/ §2.5.1 的 D3 落点）。

只用标准库。读模型落盘的 DBHub 查询结果 JSON，做三件事：

1. 把每一行归入 **缺失 / 重复 / 非法值 / 有效** 四类之一（优先级固定、互斥计数）；
2. 只用有效行计算经营指标（毛额 / 退款 / 挂起 / 净额 / 分区 / 客单价）；
3. 写 `daily.json`：计数永远覆盖全部行，明细示例受 `--max-examples` 约束，超限时
   `truncated=true` 并给出每类的 shown / total。

用法 / Usage：

    python biz_daily.py --orders orders.json
    python biz_daily.py --orders orders.json --output runs/20261025T090000/daily.json \
        --now 2026-10-25T09:00:00+08:00 --max-examples 5

设计约束（Why）：

* **数字只由脚本算**：模型负责解释与写日报，不得口算、不得补造（模块卡 C-12）。
* **四类互斥**：`missing + duplicate + invalid + valid == total` 恒成立，同一行不会
  被数两遍；一个坏行按「缺失 > 重复 > 非法值」的最高优先级只记一次。
* **坏行不进指标**：缺字段、重复、非法值都可能把金额算错，指标只统计有效行，并在
  日报里把剔除量写清楚——不猜、不补零。
* **计数与示例分开**：`counts` 覆盖全部行，`examples` 只用于展示；示例被截断时
  `truncated=true`，绝不把「只列了 N 条」误读成「只有 N 条」。
* **没有有效行就写 null**：`metrics.computed=false` 时金额字段一律为 `null`，
  不写 0.00——0 是事实，没算出来是缺数据，两者不得混写。
* 输入非法一律抛 `ScriptError`（CLI 非零退出），**不静默补默认值**。

The script owns every number the report quotes. It never connects to the database:
the model saves the DBHub query result to JSON, and this script turns that JSON into
quality counts plus business metrics.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import tempfile
from datetime import date, datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path

AGENT_NAME = "biz-daily"
SCHEMA_VERSION = 1
DEFAULT_MAX_EXAMPLES = 5
# 金额单位按样例业务固定；换成别的币种要在 README 里改口径，不改脚本含义。
# The sample business is CNY-denominated; the currency is part of the report contract.
CURRENCY = "CNY"

# 必填字段：任一为空（键不存在 / null / 空串）即「缺失」。顺序固定，示例里按此排序。
# Required fields; a missing key, null or blank string all count as "missing".
REQUIRED_FIELDS = ("order_id", "order_date", "region", "amount", "status")
STATUSES = ("paid", "refunded", "pending")


class ScriptError(Exception):
    """输入非法或查询结果损坏；调用方按错误处理，不吞。"""


def force_utf8_stdio() -> None:
    """把 stdout / stderr 固定为 UTF-8。

    原因：Basalt 的 shell 执行器按字节读子进程输出，再由上层按 UTF-8 解码；Windows
    控制台默认代码页（cp936 等）会让中文变成乱码。示例脚本一律自己钉住 UTF-8，不依赖
    运行环境的代码页。
    The shell executor captures raw bytes and decodes UTF-8; the examples pin UTF-8
    themselves instead of relying on the host code page.
    """
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is not None:
            reconfigure(encoding="utf-8", errors="replace")


def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def parse_timestamp(value: str) -> datetime | None:
    """解析 ISO 8601；接受 `Z` 后缀与纯日期。解析不了返回 None，由调用方报错。"""
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def parse_clock(value: str | None) -> datetime:
    """运行时钟（t0）。缺省取本机当前时间；显式给出时必须带时区偏移。

    带偏移是硬要求：没有偏移的 `--now` 会让「今天」取决于解释器所在时区，
    同一条命令在两台机器上可能算出不同日期。
    An explicit clock must carry an offset; otherwise "today" depends on the host zone.
    """
    if value is None:
        return datetime.now().astimezone()
    parsed = parse_timestamp(value)
    if parsed is None or parsed.tzinfo is None:
        raise ScriptError(f"--now 必须是带时区偏移的 ISO 8601 时间戳: {value!r}")
    return parsed


def load_json(path: Path, label: str) -> object:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ScriptError(f"读取{label}失败: {path}: {exc}") from exc
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise ScriptError(f"{label}不是合法 JSON: {path}: {exc}") from exc


def write_atomic(path: Path, text: str) -> None:
    """同目录临时文件 + 原子替换；父目录不存在时创建。

    父目录创建是必要的：模型写不了新目录（write_file 不建父目录），而
    `runs/<时间戳>/` 是脚本自己造出来的。
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temp_name = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(handle, "w", encoding="utf-8", newline="\n") as stream:
            stream.write(text)
        os.replace(temp_path, path)
    except BaseException:
        temp_path.unlink(missing_ok=True)
        raise


def is_blank(value: object) -> bool:
    """键不存在、null、空白串都算「缺失」；0 与 false 不算（它们是合法值）。"""
    if value is None:
        return True
    if isinstance(value, str) and value.strip() == "":
        return True
    return False


def text_of(value: object) -> str:
    return value.strip() if isinstance(value, str) else str(value).strip()


def parse_amount(value: object) -> tuple[Decimal | None, str | None]:
    """金额 -> (Decimal, 原因)。原因码是固定集合，日报与测试都按它判定。"""
    if isinstance(value, bool):  # bool 是 int 的子类，必须先排除
        return None, "amount_not_numeric"
    try:
        amount = Decimal(text_of(value))
    except (InvalidOperation, ValueError):
        return None, "amount_not_numeric"
    if not amount.is_finite():
        return None, "amount_not_finite"
    if amount < 0:
        return None, "amount_negative"
    return amount.quantize(Decimal("0.01")), None


def parse_order_date(value: str) -> bool:
    """`YYYY-MM-DD` 或带时间的 ISO 8601 都算合法；其余一律非法，不猜年份。"""
    text = value.strip()
    try:
        date.fromisoformat(text)
        return True
    except ValueError:
        pass
    try:
        datetime.fromisoformat(text)
        return True
    except ValueError:
        return False


def unwrap_dbhub_envelope(payload: dict) -> dict:
    """把 DBHub `execute_sql` 的真实返回解到「单条语句结果」。

    真实返回（@bytebase/dbhub 1.5.0 实测，2026-10-26）：
        {"success": true,
         "data": {"statements": [{"sql": "...", "rows": [...], "count": 3}],
                  "source_id": "default"}}
    只读拒绝（同样实测）：
        {"success": false, "error": "Read-only mode is enabled. ...",
         "code": "READONLY_VIOLATION"}

    为什么要在这里解：模型拿到的是这段文本，本脚本是「数字的唯一来源」——让模型
    自己挑 `rows` 等于让它改结构，判据就不再可复核。
    """
    if payload.get("success") is False:
        code = text_of(payload.get("code") or "")
        message = text_of(payload.get("error") or "")
        raise ScriptError(f"查询失败（{code or '服务端未给码'}）：{message or '服务端未给原因'}")
    data = payload.get("data")
    if not isinstance(data, dict) or "statements" not in data:
        return payload
    statements = data["statements"]
    if not isinstance(statements, list) or len(statements) != 1:
        count = len(statements) if isinstance(statements, list) else 0
        raise ScriptError(f"execute_sql 应只返回一条语句的结果，实际 {count} 条")
    statement = statements[0]
    if not isinstance(statement, dict):
        raise ScriptError(f"statements[0] 不是对象，实际 {type(statement).__name__}")
    return statement


def normalize_payload(payload: object) -> list[object]:
    """接受裸数组、`{"rows": [...]}` / `{"items": [...]}`，或 DBHub `execute_sql` 的
    真实返回（`{"success": true, "data": {"statements": [{"rows": [...]}]}}`）。"""
    if isinstance(payload, dict):
        payload = unwrap_dbhub_envelope(payload)
    if isinstance(payload, dict):
        for key in ("rows", "items"):
            if key in payload:
                payload = payload[key]
                break
        else:
            raise ScriptError(
                '查询结果必须是行数组，或 {"rows": [...]} / {"items": [...]}，'
                '或 DBHub execute_sql 的 {"success": true, "data": {"statements": [...]}} 形态'
            )
    if not isinstance(payload, list):
        raise ScriptError(f"查询结果必须是数组，实际 {type(payload).__name__}")
    for index, row in enumerate(payload, start=1):
        if not isinstance(row, dict):
            raise ScriptError(f"第 {index} 行不是对象，实际 {type(row).__name__}")
    return payload


def classify(rows: list[object]) -> dict:
    """把每行归入四类之一；返回计数、示例与有效行。

    优先级固定为 **缺失 > 重复 > 非法值**：同一行只记一次，四类之和恒等于总行数。
    重复只在前两类都通过的行之间判定——缺字段的行进不了统计，也就没有「占号」。
    Fixed precedence (missing > duplicate > invalid) keeps the four classes
    mutually exclusive and their sum equal to the row count.
    """
    counts = {"valid": 0, "missing": 0, "duplicate": 0, "invalid": 0}
    missing = {"by_field": {}, "examples": []}
    duplicate = {"order_ids": [], "examples": []}
    invalid = {"by_field": {}, "by_reason": {}, "examples": []}
    valid_rows: list[dict] = []
    first_seen: dict[str, int] = {}
    duplicated_ids: set[str] = set()

    for position, row in enumerate(rows, start=1):
        order_id = None if is_blank(row.get("order_id")) else text_of(row["order_id"])

        # 1) 缺失：任一必填字段为空。字段级计数与行级计数分开记。
        missing_fields = [field for field in REQUIRED_FIELDS if is_blank(row.get(field))]
        if missing_fields:
            counts["missing"] += 1
            for field in missing_fields:
                missing["by_field"][field] = missing["by_field"].get(field, 0) + 1
            missing["examples"].append({"row": position, "order_id": order_id, "fields": missing_fields})
            continue

        # 2) 重复：单号此前出现过（以首次出现为准，后续副本一律算重复）。
        if order_id in first_seen:
            counts["duplicate"] += 1
            duplicated_ids.add(order_id)
            duplicate["examples"].append(
                {"row": position, "order_id": order_id, "first_row": first_seen[order_id]}
            )
            continue
        first_seen[order_id] = position

        # 3) 非法值：字段都在，但内容不合约定。逐字段判定，原因码固定。
        bad_fields: list[str] = []
        reasons: list[str] = []
        amount, amount_reason = parse_amount(row["amount"])
        if amount_reason is not None:
            bad_fields.append("amount")
            reasons.append(amount_reason)
        if not parse_order_date(text_of(row["order_date"])):
            bad_fields.append("order_date")
            reasons.append("order_date_unparseable")
        status = text_of(row["status"])
        if status not in STATUSES:
            bad_fields.append("status")
            reasons.append("status_unknown")
        if reasons:
            counts["invalid"] += 1
            for field in bad_fields:
                invalid["by_field"][field] = invalid["by_field"].get(field, 0) + 1
            for reason in reasons:
                invalid["by_reason"][reason] = invalid["by_reason"].get(reason, 0) + 1
            invalid["examples"].append(
                {"row": position, "order_id": order_id, "fields": bad_fields, "reasons": reasons}
            )
            continue

        # 4) 有效：进入指标计算的唯一来源。
        counts["valid"] += 1
        valid_rows.append(
            {
                "order_id": order_id,
                "order_date": text_of(row["order_date"]),
                "region": text_of(row["region"]),
                "channel": "" if row.get("channel") is None else text_of(row["channel"]),
                "amount": amount,
                "status": status,
            }
        )

    duplicate["order_ids"] = sorted(duplicated_ids)
    return {
        "counts": counts,
        "missing": missing,
        "duplicate": duplicate,
        "invalid": invalid,
        "valid_rows": valid_rows,
    }


def money(value: Decimal | None) -> float | None:
    if value is None:
        return None
    return float(value.quantize(Decimal("0.01")))


def compute_metrics(valid_rows: list[dict]) -> dict:
    """只用有效行算指标；一条有效行都没有时金额为 null，不写 0.00 冒充「没有收入」。"""
    status_counts = {status: 0 for status in STATUSES}
    gross = Decimal("0")
    refunded = Decimal("0")
    pending = Decimal("0")
    regions: dict[str, dict] = {}
    for row in valid_rows:
        status = row["status"]
        status_counts[status] += 1
        bucket = regions.setdefault(row["region"], {"orders": 0, "net": Decimal("0")})
        bucket["orders"] += 1
        if status == "paid":
            gross += row["amount"]
            bucket["net"] += row["amount"]
        elif status == "refunded":
            refunded += row["amount"]
            bucket["net"] -= row["amount"]
        else:
            pending += row["amount"]
    net = gross - refunded
    paid_count = status_counts["paid"]
    average = (gross / paid_count).quantize(Decimal("0.01")) if paid_count else None
    computed = bool(valid_rows)
    return {
        "computed": computed,
        "valid_orders": len(valid_rows),
        "status_counts": status_counts,
        "gross_amount": money(gross) if computed else None,
        "refunded_amount": money(refunded) if computed else None,
        "pending_amount": money(pending) if computed else None,
        "net_amount": money(net) if computed else None,
        "average_paid_amount": money(average),
        "currency": CURRENCY,
        "by_region": (
            [
                {"region": region, "orders": bucket["orders"], "net_amount": money(bucket["net"])}
                for region, bucket in sorted(regions.items())
            ]
            if computed
            else []
        ),
    }


def cap(entries: list[dict], limit: int) -> tuple[list[dict], dict | None]:
    """明细示例按上限截断；返回 (展示项, 截断记录)。计数不受上限影响。"""
    if len(entries) <= limit:
        return entries, None
    return entries[:limit], {"shown": limit, "total": len(entries)}


def build_message(counts: dict, metrics: dict) -> str:
    total = sum(counts.values())
    if total == 0:
        return "本次 0 行：输入为空，未计算任何指标"
    if counts["valid"] == 0:
        return (
            f"本次 {total} 行中没有可用行"
            f"（缺失 {counts['missing']} / 重复 {counts['duplicate']} / 非法 {counts['invalid']}），指标未计算"
        )
    return (
        f"本次 {total} 行中有效 {counts['valid']} 行"
        f"（缺失 {counts['missing']} / 重复 {counts['duplicate']} / 非法 {counts['invalid']}），"
        f"净额 {metrics['net_amount']:.2f} {CURRENCY}"
    )


def daily(payload: object, *, now: str, max_examples: int = DEFAULT_MAX_EXAMPLES) -> dict:
    """纯函数入口：查询结果 -> daily.json 内容（不含 input 段，由 CLI 补路径与哈希）。"""
    if max_examples < 0:
        raise ScriptError(f"--max-examples 必须 ≥ 0，实际 {max_examples}")
    rows = normalize_payload(payload)
    classified = classify(rows)
    counts = classified["counts"]
    metrics = compute_metrics(classified["valid_rows"])

    missing_examples, missing_cut = cap(classified["missing"]["examples"], max_examples)
    duplicate_examples, duplicate_cut = cap(classified["duplicate"]["examples"], max_examples)
    invalid_examples, invalid_cut = cap(classified["invalid"]["examples"], max_examples)
    truncation = {
        name: record
        for name, record in (
            ("missing", missing_cut),
            ("duplicate", duplicate_cut),
            ("invalid", invalid_cut),
        )
        if record is not None
    }

    return {
        "schema_version": SCHEMA_VERSION,
        "observed_at": now,
        "agent": AGENT_NAME,
        "quality": {
            "total": len(rows),
            "counts": counts,
            "missing": {"by_field": classified["missing"]["by_field"], "examples": missing_examples},
            "duplicate": {
                "order_ids": classified["duplicate"]["order_ids"],
                "examples": duplicate_examples,
            },
            "invalid": {
                "by_field": classified["invalid"]["by_field"],
                "by_reason": classified["invalid"]["by_reason"],
                "examples": invalid_examples,
            },
            "truncated": bool(truncation),
            "truncation": truncation,
        },
        "metrics": metrics,
        "message": build_message(counts, metrics),
    }


def default_output(clock: datetime) -> Path:
    """缺省产物路径：runs/<时间戳>/daily.json（相对 Agent 目录，即 shell 的 cwd）。"""
    return Path("runs") / clock.strftime("%Y%m%dT%H%M%S") / "daily.json"


def main(argv: list[str] | None = None) -> int:
    force_utf8_stdio()
    parser = argparse.ArgumentParser(
        description="经营日报：把 DBHub 查询结果折算成数据质量计数与经营指标（产物不进版本库）。"
    )
    parser.add_argument("--orders", required=True, type=Path, help="模型落盘的查询结果 JSON")
    parser.add_argument("--output", type=Path, default=None, help="daily.json 路径；缺省 runs/<时间戳>/daily.json")
    parser.add_argument("--now", default=None, help="运行时钟（ISO 8601，带时区偏移）；缺省取本机当前时间")
    parser.add_argument(
        "--max-examples",
        type=int,
        default=DEFAULT_MAX_EXAMPLES,
        help=f"每类异常最多展示的明细条数（缺省 {DEFAULT_MAX_EXAMPLES}；0 表示只计数不列明细）",
    )
    args = parser.parse_args(argv)

    try:
        payload = load_json(args.orders, "查询结果")
        clock = parse_clock(args.now)
        output: Path = args.output if args.output is not None else default_output(clock)
        result = daily(payload, now=clock.isoformat(), max_examples=args.max_examples)
    except ScriptError as exc:
        # 输入非法：一个字节都不写，非零退出把判断权交给模型。
        print(f"biz_daily: {exc}", file=sys.stderr)
        return 1

    result["input"] = {"path": str(args.orders), "sha256": sha256_of(args.orders)}
    try:
        write_atomic(output, json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    except OSError as exc:
        print(f"biz_daily: 写入失败: {exc}", file=sys.stderr)
        return 1

    summary = {
        "input": result["input"],
        "report": {"path": str(output), "sha256": sha256_of(output)},
        "observed_at": result["observed_at"],
        "quality": {
            "total": result["quality"]["total"],
            "counts": result["quality"]["counts"],
            "truncated": result["quality"]["truncated"],
        },
        "metrics": {
            "computed": result["metrics"]["computed"],
            "net_amount": result["metrics"]["net_amount"],
            "currency": result["metrics"]["currency"],
        },
        "message": result["message"],
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
