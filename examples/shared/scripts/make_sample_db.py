#!/usr/bin/env python3
"""用固定输入生成本仓库示例用的 SQLite 样例库（D3 `biz-daily` / 模板 `ticket-insights`）。

只用标准库（`sqlite3`）。仓库里**不提交** `.db` 二进制：样例库是生成物，输入是
`fixtures/schema.sql`（结构）与 `fixtures/rows.json`（行数据），两者都进版本库。

用法 / Usage：

    python examples/shared/scripts/make_sample_db.py \
        --schema examples/daily/biz-daily/fixtures/schema.sql \
        --rows   examples/daily/biz-daily/fixtures/rows.json \
        --out    D:/basalt-samples/biz-sample.db --force

为什么要这个脚本：

* **可复现**：同一个 `rows.json` 在任何机器上生成逐字节语义一致的库（行序、NULL、
  文本类型的坏值都不变），六段证据里的「输入快照哈希」因此可查。
* **坏数据必须能入库**：样例库要装下「缺失 / 重复 / 非法值」三类异常，所以 schema
  故意不写 UNIQUE / CHECK / NOT NULL（见 schema.sql 顶部注释）；生成器也不做数据清洗。
* `rows.json` 里缺键的字段按 SQL NULL 入库——「字段缺失」在库里表现为 NULL，
  这正是数据质量检查要看到的样子。

Why a separate generator: the sample DB is a build artifact; the fixture JSON and DDL are
the source of truth. Nothing here cleans the data — the anomalies are the point.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
import sys
from pathlib import Path


class ScriptError(Exception):
    """输入非法；调用方按错误处理，不吞。"""


def force_utf8_stdio() -> None:
    """把 stdout / stderr 固定为 UTF-8（Basalt shell 执行器按 UTF-8 解码子进程输出）。"""
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is not None:
            reconfigure(encoding="utf-8", errors="replace")


def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_rows(path: Path) -> dict:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ScriptError(f"读取行数据失败: {path}: {exc}") from exc
    try:
        payload = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ScriptError(f"行数据不是合法 JSON: {path}: {exc}") from exc
    if not isinstance(payload, dict):
        raise ScriptError('行数据必须是 {"table", "columns", "rows"} 形态的对象')
    for key in ("table", "columns", "rows"):
        if key not in payload:
            raise ScriptError(f"行数据缺少字段: {key}")
    columns = payload["columns"]
    if not isinstance(columns, list) or not columns or not all(isinstance(c, str) for c in columns):
        raise ScriptError("columns 必须是非空字符串数组")
    if not isinstance(payload["rows"], list):
        raise ScriptError("rows 必须是数组")
    for index, row in enumerate(payload["rows"], start=1):
        if not isinstance(row, dict):
            raise ScriptError(f"第 {index} 行不是对象，实际 {type(row).__name__}")
        unknown = sorted(set(row) - set(columns))
        if unknown:
            raise ScriptError(f"第 {index} 行出现未声明的列: {unknown}")
    return payload


def build(schema: Path, rows_path: Path, out: Path, *, force: bool) -> dict:
    if out.exists() and not force:
        raise ScriptError(f"目标库已存在（加 --force 覆盖）: {out}")
    payload = load_rows(rows_path)
    try:
        ddl = schema.read_text(encoding="utf-8")
    except OSError as exc:
        raise ScriptError(f"读取结构文件失败: {schema}: {exc}") from exc

    out.parent.mkdir(parents=True, exist_ok=True)
    if out.exists():
        out.unlink()  # --force：先删再建，避免旧文件里残留被 schema 删掉的表
    connection = sqlite3.connect(out)
    try:
        connection.executescript(ddl)
        table = payload["table"]
        columns = payload["columns"]
        exists = connection.execute(
            "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", (table,)
        ).fetchone()
        if exists is None:
            raise ScriptError(f"schema 没有创建表: {table}")
        placeholders = ", ".join("?" for _ in columns)
        statement = f'INSERT INTO "{table}" ({", ".join(columns)}) VALUES ({placeholders})'
        connection.executemany(
            statement,
            [[row.get(column) for column in columns] for row in payload["rows"]],
        )
        connection.commit()
        stored = connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
    finally:
        connection.close()

    if stored != len(payload["rows"]):
        raise ScriptError(f"入库行数不符：期望 {len(payload['rows'])}，实际 {stored}")
    return {
        "db": {"path": str(out), "sha256": sha256_of(out)},
        "schema": {"path": str(schema), "sha256": sha256_of(schema)},
        "rows": {"path": str(rows_path), "sha256": sha256_of(rows_path), "count": stored},
        "table": payload["table"],
    }


def main(argv: list[str] | None = None) -> int:
    force_utf8_stdio()
    parser = argparse.ArgumentParser(
        description="用 schema.sql + rows.json 生成 SQLite 样例库（生成物，不进版本库）。"
    )
    parser.add_argument("--schema", required=True, type=Path, help="DDL 文件")
    parser.add_argument("--rows", required=True, type=Path, help='行数据 JSON（{"table", "columns", "rows"}）')
    parser.add_argument("--out", required=True, type=Path, help="输出 .db 路径（建议放在仓库外）")
    parser.add_argument("--force", action="store_true", help="覆盖已存在的目标库")
    args = parser.parse_args(argv)

    try:
        summary = build(args.schema, args.rows, args.out, force=args.force)
    except ScriptError as exc:
        print(f"make_sample_db: {exc}", file=sys.stderr)
        return 1
    except sqlite3.Error as exc:
        print(f"make_sample_db: SQLite 失败: {exc}", file=sys.stderr)
        return 1

    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
