"""US19 本地演示用量保护；只转发协议，不执行 Agent 或工具。

Local demonstration budget guard, not a product execution engine. Real use is
opt-in; credentials remain in process environment. All trials share one journal.
"""

import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import threading
import time
import urllib.error
import urllib.request
from decimal import Decimal, ROUND_CEILING
from http.server import BaseHTTPRequestHandler, HTTPServer


LIMITS = {"calls": 90, "input": 1_000_000, "output": 80_000, "cost_micro_cny": 30_000_000}
# 使用北京区已核实的最高档单价作保守估算，不扣缓存／免费额度。
# Conservative upper prices in micro-CNY per token; never claim the actual bill.
PRICES = {"chat": (4.8, 64), "embedding": (0.5, 0)}
PRICE_SOURCES = [
    "https://help.aliyun.com/zh/model-studio/qwen-plus",
    "https://help.aliyun.com/zh/model-studio/text-embedding-v4",
]


class Refused(Exception):
    """本次请求在外发前拒绝。Request rejected before dispatch."""


def upper_cost(kind, incoming, outgoing):
    amount = Decimal(str(PRICES[kind][0])) * incoming + Decimal(str(PRICES[kind][1])) * outgoing
    return int(amount.to_integral_value(rounding=ROUND_CEILING))


class Ledger:
    """预占先落盘，结算未知即全局关闭。Persist reservation before forwarding."""

    def __init__(self, path, authorized_chat_limit=90):
        if authorized_chat_limit not in (90, 115, 125):
            raise Refused("unsupported explicit authorization")
        self.path = Path(path)
        self.lock = threading.Lock()
        self.limits = dict(LIMITS, calls=authorized_chat_limit)
        self.state = json.loads(self.path.read_text(encoding="utf-8")) if self.path.exists() else {
            "version": 1, "authorization": "US19 user 2026-11-03 qwen-plus 90 calls / 1M input / 80K output / CNY30 incl embedding",
            "price_checked_at": "2026-11-03", "price_sources": PRICE_SOURCES,
            "price_method": "Beijing published highest-tier upper estimate; not an invoice",
            "limits": LIMITS, "halted": False, "entries": [],
        }
        if self.state["limits"] != self.limits:
            # 只接受用户已批准的逐次追加，保留原授权、所有消费和未知状态。
            # Explicit amendment preserves the original authorization and every entry.
            previous_limit = {115: 90, 125: 115}.get(authorized_chat_limit)
            if (previous_limit is None or self.state["limits"] != dict(LIMITS, calls=previous_limit)
                    or self.state["halted"] or any(row["state"] != "settled" for row in self.state["entries"])):
                raise Refused("journal limits differ or usage unresolved; do not reset authorization")
            self.state.setdefault("authorization_amendments", []).append({
                "approved_at": "2026-11-04", "from_chat_calls": previous_limit, "to_chat_calls": authorized_chat_limit,
                "scope": "one corrected business verification; other cumulative caps unchanged",
                "retained_entries": len(self.state["entries"]),
            })
            self.state["limits"] = self.limits
            self.save()
        if self.state["limits"] != self.limits:
            raise Refused("journal limits differ; do not reset accumulated authorization")
        if any(row["state"] == "reserved" for row in self.state["entries"]):
            self.state["halted"] = True
            self.save()

    def save(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self.path.with_suffix(".pending")
        with temporary.open("w", encoding="utf-8") as stream:
            json.dump(self.state, stream, ensure_ascii=False, indent=2)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, self.path)

    def reserve(self, kind, body):
        encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
        # 字节上界另加协议余量；结算超出预占必须关闭，不能当零消费重试。
        # Reserve conservatively from serialized UTF-8 bytes plus protocol overhead.
        incoming = 2 * len(encoded) + 4096
        outgoing = body["max_tokens"] if kind == "chat" else 0
        cost = upper_cost(kind, incoming, outgoing)
        with self.lock:
            if self.state["halted"]:
                raise Refused("budget ledger halted: reconcile unknown usage first")
            rows = self.state["entries"]
            if any(row["state"] in ("reserved", "unknown") for row in rows):
                raise Refused("previous request has no confirmed usage yet")
            totals = {"calls": sum(row["kind"] == "chat" for row in rows),
                      "input": sum(row["input"] for row in rows),
                      "output": sum(row["output"] for row in rows),
                      "cost_micro_cny": sum(row["cost_micro_cny"] for row in rows)}
            requested = {"calls": int(kind == "chat"), "input": incoming,
                         "output": outgoing, "cost_micro_cny": cost}
            if any(totals[key] + requested[key] > cap for key, cap in self.limits.items()):
                raise Refused("authorized cumulative budget would be exceeded")
            row = {"id": len(rows) + 1, "kind": kind, "state": "reserved", "at": time.time(),
                   "request_sha256": hashlib.sha256(encoded).hexdigest(), "model": body["model"],
                   "input": incoming, "output": outgoing, "cost_micro_cny": cost}
            rows.append(row)
            self.save()
            return row["id"]

    def finish(self, ticket, response):
        with self.lock:
            row = self.state["entries"][ticket - 1]
            usage = response.get("usage") if isinstance(response, dict) else None
            incoming = usage.get("prompt_tokens", usage.get("total_tokens")) if isinstance(usage, dict) else None
            outgoing = usage.get("completion_tokens") if row["kind"] == "chat" and isinstance(usage, dict) else 0
            valid = type(incoming) is int and incoming > 0 and type(outgoing) is int and outgoing >= 0
            valid = valid and incoming <= row["input"] and outgoing <= row["output"]
            if not valid:
                row["state"] = "unknown"
                self.state["halted"] = True
                self.save()
                raise Refused("usage missing or exceeds reservation; halted without replay")
            row.update(state="settled", input=incoming, output=outgoing,
                       cost_micro_cny=upper_cost(row["kind"], incoming, outgoing),
                       elapsed_ms=round((time.time() - row["at"]) * 1000))
            self.save()

    def unknown(self, ticket):
        with self.lock:
            self.state["entries"][ticket - 1]["state"] = "unknown"
            self.state["halted"] = True
            self.save()


def prepare(path, raw, key):
    """限制模型、数据、非流式和输出预算；无隐式模型降级。Freeze permitted protocol."""
    if len(raw) > 300_000:
        raise Refused("request too large")
    text = raw.decode("utf-8")
    if any(value and value in text for value in [key, "US19_PRIVATE_CANARY", "普陀区政务大模型"]):
        raise Refused("unapproved source or credential canary detected")
    body = json.loads(text)
    canonical = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
    if any(value and value in canonical for value in [key, "US19_PRIVATE_CANARY", "普陀区政务大模型"]):
        raise Refused("decoded unapproved source or credential canary detected")
    if not isinstance(body, dict):
        raise Refused("request must be an object")
    if path == "/v1/chat/completions" and body.get("model") == "qwen-plus":
        if set(body) - {"model", "messages", "stream", "max_tokens", "temperature", "tools", "tool_choice"}:
            raise Refused("unbudgeted chat protocol fields")
        if body.get("stream"):
            raise Refused("streaming not authorized for this measurement")
        cap = body.get("max_tokens", 1800)
        if type(cap) is not int or not 1 <= cap <= 2400:
            raise Refused("invalid output reservation")
        body.update(max_tokens=cap, enable_thinking=False, stream=False)
        return "chat", body
    if path == "/v1/embeddings" and body.get("model") == "text-embedding-v4":
        if set(body) - {"model", "input", "dimensions", "encoding_format"}:
            raise Refused("unbudgeted embedding protocol fields")
        inputs = body.get("input")
        if not isinstance(inputs, (list, str)) or not inputs or isinstance(inputs, list) and len(inputs) > 10:
            raise Refused("invalid embedding batch")
        return "embedding", body
    raise Refused("path or model not authorized")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, url):
        raise Refused("upstream redirect refused")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-real", action="store_true")
    parser.add_argument("--journal", required=True)
    parser.add_argument("--ready", required=True)
    parser.add_argument("--listen-port", type=int, default=0, help="loopback port; zero selects an available port")
    parser.add_argument("--authorized-chat-limit", type=int, choices=(90, 115, 125), default=90,
                        help="115 and 125 require their explicit user amendments; all other caps stay fixed")
    args = parser.parse_args()
    if not 0 <= args.listen_port <= 65535:
        parser.error("invalid loopback port")
    if not args.allow_real:
        parser.error("explicit authorized --allow-real required")
    key = os.environ.get("DASHSCOPE_API_KEY", "")
    local_token = os.environ.get("BASALT_US19_GATEWAY_TOKEN", "")
    base = os.environ.get("BASALT_DEV_EMBEDDING_BASE_URL", "").rstrip("/")
    if not key or len(local_token) < 32 or not re.fullmatch(r"https://[a-z0-9-]+\.cn-beijing\.maas\.aliyuncs\.com/compatible-mode/v1", base):
        parser.error("protected Beijing workspace credential/config unavailable")
    # 同一累计账本只允许一个本地网关；崩溃留下锁，人工查证前不自动夺锁。
    # A crash leaves the exclusive lock intact; never reset authorization implicitly.
    lock_path = Path(args.journal + ".lock")
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open("x", encoding="ascii") as lock_file:
        lock_file.write(str(os.getpid()))
    ledger = Ledger(args.journal, args.authorized_chat_limit)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            return

        def do_POST(self):
            ticket = None
            try:
                if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + local_token):
                    self.send_response(401)
                    self.end_headers()
                    return
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 300_000:
                    raise Refused("invalid content length")
                kind, body = prepare(self.path, self.rfile.read(length), key)
                if os.environ.get("CI") or os.environ.get("GITHUB_ACTIONS"):
                    raise Refused("real model dispatch is forbidden in CI")
                ticket = ledger.reserve(kind, body)
                payload = json.dumps(body, ensure_ascii=False).encode()
                # Only the isolated demo's approved public/synthetic request body is retained.
                # 凭据/canary 已检查；保留精确请求供复核全部模型可见上下文，不保存请求头。
                request_dir = Path(args.journal).with_suffix(".requests")
                request_dir.mkdir(parents=True, exist_ok=True)
                with (request_dir / f"{ticket:04d}.json").open("xb") as saved_request:
                    saved_request.write(payload)
                    saved_request.flush()
                    os.fsync(saved_request.fileno())
                request = urllib.request.Request(base + self.path.removeprefix("/v1"), data=payload,
                    headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
                with opener.open(request, timeout=100) as upstream:
                    raw_response = upstream.read(12_000_001)
                if len(raw_response) > 12_000_000:
                    raise Refused("response exceeds limit")
                ledger.finish(ticket, json.loads(raw_response))
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(raw_response)
            except (Refused, ValueError, OSError, urllib.error.URLError):
                if ticket is not None and ledger.state["entries"][ticket - 1]["state"] == "reserved":
                    ledger.unknown(ticket)
                self.send_response(429 if ticket is None else 502)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"error":{"message":"US19 budget or evidence guard stopped this request; inspect local journal"}}')

    # 串行 HTTP 保证一次只有一个已预占、尚未结算的外发。
    # One in-flight request; queued clients never bypass a pending reservation.
    server = HTTPServer(("127.0.0.1", args.listen_port), Handler)
    Path(args.ready).write_text(json.dumps({"base_url": "http://127.0.0.1:" + str(server.server_port) + "/v1", "pid": os.getpid(), "journal": args.journal}), encoding="utf-8")
    try:
        server.serve_forever()
    finally:
        server.server_close()
        lock_path.unlink()


if __name__ == "__main__":
    main()
