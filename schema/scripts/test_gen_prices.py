"""Tests for gen_prices.py: python3 -m unittest discover -s schema/scripts"""
import copy
import os
import tempfile
import unittest

import gen_prices as gen

M = 1e-6  # upstream prices are per token


def spec(provider, inp, out, cache_read=None, cw5=None, cw1h=None, **extra):
    s = {"litellm_provider": provider, "mode": "chat",
         "input_cost_per_token": inp * M, "output_cost_per_token": out * M}
    for field, v in (("cache_read_input_token_cost", cache_read),
                     ("cache_creation_input_token_cost", cw5),
                     ("cache_creation_input_token_cost_above_1hr", cw1h)):
        if v is not None:
            s[field] = v * M
    s.update({k: v * M for k, v in extra.items()})
    return s


# Every canary's upstream entry, at its real prices.
UPSTREAM = {
    "claude-opus-5": spec("anthropic", 5, 25, 0.5, 6.25, 10),
    "claude-opus-5-5": spec("anthropic", 4, 20, 0.2, 5, 8),
    "claude-fable-5-1": spec("anthropic", 10, 50, 0.25, 12.5, 20),
    "gpt-5": spec("openai", 1.25, 10, 0.125),
    "claude-sonnet-4-5": spec(
        "anthropic", 3, 15, 0.3, 3.75, 6,
        input_cost_per_token_above_200k_tokens=6,
        output_cost_per_token_above_200k_tokens=22.5,
        cache_read_input_token_cost_above_200k_tokens=0.6,
        cache_creation_input_token_cost_above_200k_tokens=7.5,
        cache_creation_input_token_cost_above_1hr_above_200k_tokens=12),
    "gpt-5.6-sol": spec(
        "openai", 4, 20, 0.4, 5,
        input_cost_per_token_above_272k_tokens=8,
        output_cost_per_token_above_272k_tokens=30,
        cache_read_input_token_cost_above_272k_tokens=0.8,
        input_cost_per_token_above_272k_tokens_priority=16),
}


class GenPricesTest(unittest.TestCase):
    def setUp(self):
        # check() compares against schema/prices.json under the working
        # directory; an empty one keeps the real table out of these tests.
        self.cwd = os.getcwd()
        self.tmp = tempfile.TemporaryDirectory()
        os.chdir(self.tmp.name)

    def tearDown(self):
        os.chdir(self.cwd)
        self.tmp.cleanup()

    def check(self, raw):
        gen.check(gen.build(raw), raw)

    def test_the_real_canaries_pass(self):
        self.check(UPSTREAM)

    def test_a_renamed_field_is_refused_even_when_the_fallback_matches(self):
        raw = copy.deepcopy(UPSTREAM)
        opus = raw["claude-opus-5"]
        opus["cache_read_cost"] = opus.pop("cache_read_input_token_cost")
        self.assertEqual(gen.build(raw)["|claude-opus-5"]["cache_read"], 0.5)
        with self.assertRaisesRegex(SystemExit, "cache_read_input_token_cost"):
            self.check(raw)

    def test_a_renamed_tier_field_is_refused(self):
        raw = copy.deepcopy(UPSTREAM)
        sol = raw["gpt-5.6-sol"]
        sol["input_cost_per_token_above_272k"] = sol.pop("input_cost_per_token_above_272k_tokens")
        with self.assertRaisesRegex(SystemExit, "no tier above 272k"):
            self.check(raw)

    def test_missing_claude_cache_writes_fall_back_to_the_published_multipliers(self):
        r = gen.rate("anthropic.claude-x", spec("bedrock", 3, 15, 0.3))
        self.assertEqual((r["cache_write_5m"], r["cache_write_1h"]), (3.75, 6.0))
        r = gen.rate("anthropic.claude-y", spec("bedrock", 3, 15, 0.3, 3.75))
        self.assertEqual(r["cache_write_1h"], 6.0)

    def test_a_tier_fills_what_upstream_omits_and_skips_other_service_tiers(self):
        tiers = gen.build(UPSTREAM)["|gpt-5.6-sol"]["tiers"]
        # Cache writes scale by the tier's 2x input multiplier; the _priority
        # price is another service tier, not a second threshold.
        self.assertEqual(tiers, [{"above": 272000, "input": 8.0, "output": 30.0, "cache_read": 0.8,
                                  "cache_write_5m": 10.0, "cache_write_1h": 10.0}])
        r = gen.rate("gemini/x", spec("gemini", 1.25, 10, 0.125, input_cost_per_token_above_200k_tokens=2.5))
        self.assertEqual(r["tiers"][0]["output"], 10.0)

    def test_an_inclusive_threshold_is_one_token_lower(self):
        raw = {"xai/grok-4": spec("xai", 1.25, 2.5, input_cost_per_token_above_200k_tokens=2.5)}
        self.assertEqual(gen.build(raw)["xai|grok-4"]["tiers"][0]["above"], 199_999)

    def test_a_bare_key_is_the_makers_price_never_a_resellers(self):
        raw = {
            # A reseller listed first must not win.
            "azure_ai/grok-4": spec("azure_ai", 3, 15),
            "xai/grok-4": spec("xai", 1.25, 2.5),
            # dashscope makes Qwen, not DeepSeek; only resellers list this one.
            "dashscope/deepseek-v4-flash-0731": spec("dashscope", 0.3, 0.6),
            "dashscope/qwen-max": spec("dashscope", 1.6, 6.4),
            # An unprefixed name outranks the maker's prefixed one.
            "gemini-2.5-pro": spec("vertex_ai-language-models", 1.25, 10),
            "gemini/gemini-2.5-pro": spec("gemini", 1.3, 10),
        }
        rates = gen.build(raw)
        self.assertEqual(rates["|grok-4"]["input"], 1.25)
        self.assertNotIn("|deepseek-v4-flash-0731", rates)
        self.assertEqual(rates["|qwen-max"]["input"], 1.6)
        self.assertEqual(rates["|gemini-2.5-pro"]["input"], 1.25)
        self.assertIn("azure_ai|grok-4", rates)


if __name__ == "__main__":
    unittest.main()
