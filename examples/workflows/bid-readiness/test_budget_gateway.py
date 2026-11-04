"""Budget boundary regressions. No network or real credentials."""
import json
from pathlib import Path
import tempfile
import os
import subprocess
import sys
import time
import unittest
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor
from budget_gateway import Ledger, Refused, prepare, upper_cost


class BudgetTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / "ledger.json"
        self.ledger = Ledger(self.path)
        self.body = {"model": "qwen-plus", "max_tokens": 100, "messages": []}

    def test_reservation_is_durable_before_dispatch(self):
        ticket = self.ledger.reserve("chat", self.body)
        self.assertEqual(json.loads(self.path.read_text())["entries"][0]["state"], "reserved")
        self.ledger.finish(ticket, {"usage": {"prompt_tokens": 25, "completion_tokens": 40}})
        resumed = Ledger(self.path)
        self.assertFalse(resumed.state["halted"])
        self.assertEqual(resumed.state["entries"][0]["output"], 40)

    def test_crash_or_unknown_usage_halts_without_reset(self):
        self.ledger.reserve("chat", self.body)
        resumed = Ledger(self.path)
        with self.assertRaises(Refused):
            resumed.reserve("chat", self.body)
        self.assertEqual(len(resumed.state["entries"]), 1)

    def test_missing_and_over_reservation_usage_stop(self):
        for usage in [{}, {"prompt_tokens": 5, "completion_tokens": 101}]:
            with self.subTest(usage=usage):
                ledger = Ledger(self.path.with_name(str(len(usage)) + ".json"))
                ticket = ledger.reserve("chat", self.body)
                with self.assertRaises(Refused):
                    ledger.finish(ticket, {"usage": usage})
                with self.assertRaises(Refused):
                    ledger.reserve("chat", self.body)

    def test_concurrent_calls_share_ninety_call_limit(self):
        def attempt(_):
            try:
                ticket = self.ledger.reserve("chat", self.body)
                self.ledger.finish(ticket, {"usage": {"prompt_tokens": 5, "completion_tokens": 1}})
                return True
            except Refused:
                return False
        with ThreadPoolExecutor(max_workers=4) as pool:
            accepted = list(pool.map(attempt, range(95)))
        self.assertLessEqual(sum(accepted), 90)
        while len(self.ledger.state["entries"]) < 90:
            self.assertTrue(attempt(0))
        self.assertFalse(attempt(0))
        self.assertEqual(len(Ledger(self.path).state["entries"]), 90)

    def test_pending_live_request_blocks_next_forward_and_cost_rounds_up(self):
        self.ledger.reserve("chat", self.body)
        with self.assertRaises(Refused):
            self.ledger.reserve("chat", self.body)
        self.assertEqual(len(self.ledger.state["entries"]), 1)
        self.assertEqual(upper_cost("embedding", 1, 0), 1)

    def test_explicit_amendment_preserves_usage_and_stops_at_115(self):
        for _ in range(87):
            ticket = self.ledger.reserve("chat", self.body)
            self.ledger.finish(ticket, {"usage": {"prompt_tokens": 5, "completion_tokens": 1}})
        original = json.loads(self.path.read_text())["entries"]
        amended = Ledger(self.path, 115)
        self.assertEqual(amended.state["entries"], original)
        self.assertEqual(amended.state["limits"], {"calls": 115, "input": 1000000, "output": 80000, "cost_micro_cny": 30000000})
        self.assertEqual(len(amended.state["authorization_amendments"]), 1)
        with self.assertRaises(Refused):
            Ledger(self.path)
        for _ in range(28):
            ticket = amended.reserve("chat", self.body)
            amended.finish(ticket, {"usage": {"prompt_tokens": 5, "completion_tokens": 1}})
        resumed = Ledger(self.path, 115)
        with self.assertRaises(Refused):
            resumed.reserve("chat", self.body)
        self.assertEqual(len(resumed.state["entries"]), 115)
        self.assertEqual(len(resumed.state["authorization_amendments"]), 1)

    def test_amendment_does_not_clear_unknown_usage(self):
        self.ledger.reserve("chat", self.body)
        before = self.path.read_bytes()
        with self.assertRaises(Refused):
            Ledger(self.path, 115)
        self.assertEqual(self.path.read_bytes(), before)
        with self.assertRaises(Refused):
            Ledger(self.path, 116)

    def test_second_amendment_preserves_history_and_stops_at_125(self):
        amended = Ledger(self.path, 115)
        for _ in range(99):
            ticket = amended.reserve("chat", self.body)
            amended.finish(ticket, {"usage": {"prompt_tokens": 5, "completion_tokens": 1}})
        before = json.loads(self.path.read_text())
        final = Ledger(self.path, 125)
        self.assertEqual(final.state["entries"], before["entries"])
        self.assertEqual(final.state["authorization_amendments"][:1], before["authorization_amendments"])
        self.assertEqual(final.state["limits"], dict(before["limits"], calls=125))
        self.assertEqual(final.state["authorization_amendments"][-1]["from_chat_calls"], 115)
        self.assertEqual(final.state["authorization_amendments"][-1]["retained_entries"], 99)
        for _ in range(26):
            ticket = final.reserve("chat", self.body)
            final.finish(ticket, {"usage": {"prompt_tokens": 5, "completion_tokens": 1}})
        resumed = Ledger(self.path, 125)
        with self.assertRaises(Refused):
            resumed.reserve("chat", self.body)
        self.assertEqual(len(resumed.state["entries"]), 125)
        self.assertEqual(len(resumed.state["authorization_amendments"]), 2)
        with self.assertRaises(Refused):
            Ledger(self.path, 115)

    def test_second_amendment_refuses_unsettled_or_changed_caps(self):
        amended = Ledger(self.path, 115)
        amended.reserve("chat", self.body)
        before = self.path.read_bytes()
        for cap in (125, 126):
            with self.assertRaises(Refused):
                Ledger(self.path, cap)
            self.assertEqual(self.path.read_bytes(), before)
        amended.state["entries"] = []
        amended.state["limits"]["output"] += 1
        amended.save()
        with self.assertRaises(Refused):
            Ledger(self.path, 125)

    def test_embedding_cost_and_chat_output_cap_are_cumulative(self):
        self.ledger.state["entries"] = [{"id": 1, "kind": "embedding", "state": "settled", "input": 1, "output": 0, "cost_micro_cny": 29_999_999}]
        with self.assertRaises(Refused):
            self.ledger.reserve("chat", self.body)
        self.ledger.state["entries"] = [{"id": 1, "kind": "chat", "state": "settled", "input": 1, "output": 79_999, "cost_micro_cny": 1}]
        with self.assertRaises(Refused):
            self.ledger.reserve("chat", self.body)

    def test_protocol_rejects_private_sources_other_models_and_streaming(self):
        for extra in [{"model": "other"}, {"stream": True}, {"messages": [{"content": "US19_PRIVATE_CANARY"}]}, {"messages": [{"content": "secret-test-value"}]}, {"max_tokens": 2500}, {"n": 2}, {"max_completion_tokens": 90000}, {"enable_search": True}]:
            with self.subTest(extra=extra), self.assertRaises(Refused):
                prepare("/v1/chat/completions", json.dumps(self.body | extra).encode(), "secret-test-value")
        kind, body = prepare("/v1/chat/completions", json.dumps(self.body).encode(), "secret-test-value")
        self.assertEqual(kind, "chat")
        self.assertFalse(body["enable_thinking"])

    def test_decoded_unicode_and_nested_keys_cannot_bypass_canary(self):
        for text in ['{"model":"qwen-plus","messages":[{"content":"US19_\\u0050RIVATE_CANARY"}]}',
                     '{"model":"qwen-plus","messages":[{"content":"\\u0073ecret-test-value"}]}',
                     '{"model":"qwen-plus","nested":{"US19_\\u0050RIVATE_CANARY":1}}', '[]']:
            with self.subTest(text=text), self.assertRaises(Refused):
                prepare("/v1/chat/completions", text.encode(), "secret-test-value")

    def test_http_auth_and_invalid_protocol_never_create_reservation(self):
        ready = self.path.with_name("ready.json")
        environment = dict(os.environ, DASHSCOPE_API_KEY="offline-test-only", BASALT_US19_GATEWAY_TOKEN="local-test-token-" * 3,
                           BASALT_DEV_EMBEDDING_BASE_URL="https://offline-only.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", CI="true")
        process = subprocess.Popen([sys.executable, str(Path(__file__).with_name("budget_gateway.py")), "--allow-real", "--journal", str(self.path), "--ready", str(ready)], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
        try:
            deadline = time.monotonic() + 5
            while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.02)
            self.assertTrue(ready.exists())
            base = json.loads(ready.read_text())["base_url"]
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            invalid = b'{"model":"not-authorized"}'
            valid = b'{"model":"qwen-plus","messages":[]}'
            for token, payload, expected in [("wrong", invalid, 401), (environment["BASALT_US19_GATEWAY_TOKEN"], invalid, 429), (environment["BASALT_US19_GATEWAY_TOKEN"], valid, 429)]:
                request = urllib.request.Request(base + "/chat/completions", data=payload, headers={"Authorization": "Bearer " + token})
                with self.assertRaises(urllib.error.HTTPError) as result:
                    opener.open(request, timeout=2)
                self.assertEqual(result.exception.code, expected)
            self.assertFalse(self.path.exists(), "authentication/protocol refusal must precede reservation and network forwarding")
        finally:
            process.terminate()
            process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
