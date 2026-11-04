"""固定矩阵与演示修订离线回归，不调用真实模型。Offline revision regressions."""
import copy
import json
import unittest

import demo
import matrix_definition as definition
import matrix_report as matrix
from collect import coverage as legacy_coverage
from test_collect import fixture as legacy_fixture


def fixture(group=1):
    return {"report": {"result": {key: "依据：离线自测占位，不是业务结论；缺口：没有执行真实复核；责任：自测；行动：保留待验证" for key in matrix.expected_ids(group)},
                       "statuses": {key: "待验证" for key in matrix.expected_ids(group)},
                       "sources": "offline self-test only", "open_questions": "not real model evidence"}}


class MatrixTests(unittest.TestCase):
    def test_complete_groups_keep_all_frozen_originals(self):
        reviews = {}
        for group in range(1, 5):
            raw = json.dumps(fixture(group), ensure_ascii=False)
            reviews[f"review_{group}"] = raw
            parsed = matrix.parse_review(raw, group)
            self.assertEqual([row["id"] for row in parsed["rows"]], matrix.expected_ids(group))
            rendered = matrix.render(parsed)
            for row in parsed["rows"]:
                original, source = matrix.source_requirement(row["id"])
                self.assertIn(original.replace("\n", " "), rendered)
                self.assertIn(source, rendered)
        self.assertTrue(matrix.coverage(reviews)["structure_complete"])
        del reviews["review_4"]
        self.assertFalse(matrix.coverage(reviews)["structure_complete"])

    def test_wrong_group_missing_extra_and_invalid_status(self):
        for variant in ("wrong-group", "missing", "extra", "invalid-status", "missing-handoff"):
            value = fixture()
            if variant == "wrong-group":
                value["report"]["result"]["R13"] = value["report"]["result"].pop("R01")
            elif variant == "missing":
                del value["report"]["statuses"]["R01"]
            elif variant == "extra":
                value["report"]["result"]["R99"] = "unexpected"
            elif variant == "invalid-status":
                value["report"]["statuses"]["R01"] = "自动通过"
            else:
                del value["report"]["sources"]
            with self.subTest(variant=variant), self.assertRaises(ValueError):
                matrix.parse_review(json.dumps(value, ensure_ascii=False), 1)

    def test_blank_text_and_each_missing_field(self):
        for analysis in ("", " \n\t", "只给一句任意结论", "依据： ；缺口：缺；责任：人；行动：补", "依据：有；缺口： ；责任：人；行动：补", "依据：有；缺口：缺；责任： ；行动：补", "依据：有；缺口：缺；责任：人；行动：  "):
            value = fixture()
            value["report"]["result"]["R01"] = analysis
            with self.subTest(analysis=analysis), self.assertRaises(ValueError):
                matrix.parse_review(json.dumps(value, ensure_ascii=False), 1)
        for field in ("sources", "open_questions"):
            value = fixture()
            value["report"][field] = " \n\t"
            with self.subTest(field=field), self.assertRaises(ValueError):
                matrix.parse_review(json.dumps(value, ensure_ascii=False), 1)

    def test_duplicate_json_is_not_last_value_wins(self):
        raw = json.dumps(fixture(), ensure_ascii=False)
        for injected in ('{"report":{},' + raw[1:], raw.replace('"R01":', '"R01":"fake","R01":', 1)):
            with self.assertRaises(ValueError):
                matrix.parse_review(injected, 1)

    def test_legacy_format_is_never_regraded(self):
        legacy = legacy_fixture()
        self.assertTrue(legacy_coverage(legacy)["structure_complete"])
        legacy["review_1"] = legacy["review_1"].replace("R02|", "R01|", 1)
        self.assertFalse(legacy_coverage(legacy)["structure_complete"])
        for group in range(1, 5):
            with self.assertRaises(ValueError):
                matrix.parse_review(legacy[f"review_{group}"], group)

    def test_nonempty_fabrication_is_not_business_proof(self):
        value = fixture()
        value["report"]["result"]["R01"] = "依据：虚构了产品认证；缺口：声称没有；责任：自测；行动：待独立核对"
        parsed = matrix.parse_review(json.dumps(value, ensure_ascii=False), 1)
        self.assertIn("not established", parsed["business_correctness"])

    def test_revision_preserves_budget_inputs_and_original_definition(self):
        roles = json.loads((demo.ROOT / "roles.json").read_text(encoding="utf-8"))
        original = demo.build_definitions({name: {"name": name} for name in roles})["long"]
        before = copy.deepcopy(original)
        revised = definition.build(original, demo.ROOT)
        self.assertEqual(original, before)
        self.assertEqual(revised["limits"], original["limits"])
        self.assertEqual(len(revised["nodes"]), 17)
        self.assertLessEqual(definition.count_properties(revised), 1024)
        for node in revised["nodes"]:
            if node["id"].startswith(("review_", "delivery_", "technical_")):
                group = int(node["id"].split("_")[1])
                text = next(item["literal"] for item in node["inputs"] if item["target"] == "instruction")
                for chapter in (group * 2 - 1, group * 2):
                    self.assertIn(definition.frozen_source(demo.ROOT, f"request-{chapter:02d}.md"), text)
            if node["id"].startswith("review_"):
                for filename in ("product-main.md", "commercial-synthetic.md"):
                    self.assertIn(definition.frozen_source(demo.ROOT, filename), text)
                self.assertEqual(node["output_schema"], matrix.matrix_schema(group))

    def test_narrowing_does_not_change_tools_or_memory_policy(self):
        for name in ("bid-delivery", "bid-reviewer"):
            original = (demo.ROOT / "agents" / name / "AGENT.md").read_text(encoding="utf-8")
            result = definition.narrow_agent(original)
            self.assertIn("knowledge: []", result)
            self.assertIn("include_global_memory: false", result)
            self.assertIn("tools:\n  - read_file", result)
            self.assertIn("max_iterations: 4", result)
            self.assertIn(original.split("---", 2)[2], result)
            with self.assertRaises(ValueError):
                definition.narrow_agent(result)
        binding = "resource_bindings:\n  # e2e-fixture-bindings\n  model_connection:\n    - name: demo-qwen\n      uid: rs_FIXTURE123\n"
        bound = original.replace("\n---\n", "\n" + binding + "---\n", 1)
        self.assertEqual(definition.without_fixture_binding(bound), original)
        self.assertIn(binding, definition.narrow_agent(bound))
        self.assertNotEqual(definition.without_fixture_binding(bound.replace("demo-qwen", "other-model")), original)


if __name__ == "__main__":
    unittest.main()
