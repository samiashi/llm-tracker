#!/usr/bin/env python3
"""Generate schema/prices.json from LiteLLM's price database.

LiteLLM's model_prices_and_context_window.json is the de-facto community source
for model pricing, and it tracks the details that matter here: the 5m/1h
cache-write split, per-model cache-read multipliers such as Fable 5.1's 0.025x,
and long-context tiers.

Run `make prices` to refresh from upstream. Passing a path reads a saved copy
of the upstream file instead, so a regeneration can be reproduced exactly:

    python3 schema/scripts/gen_prices.py litellm.json

Only the providers that can appear in a coding-agent log are kept, so the
embedded table stays small enough to ship inside the binary.
"""
import json
import re
import sys
import urllib.error
import urllib.request

SOURCE = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

# Relative to the repository root, where `make prices` runs.
PRICES = "schema/prices.json"

# Providers a coding harness can plausibly report. Everything else is dropped.
KEEP_PROVIDERS = {
    "anthropic", "openai", "text-completion-openai",
    "deepseek", "moonshot", "zai", "openrouter", "xai", "mistral",
    "gemini", "vertex_ai-language-models", "groq", "together_ai", "fireworks_ai",
}

# Substrings that keep a model even when its provider is not in the list above.
KEEP_SUBSTRINGS = ("claude", "gpt-", "glm", "kimi", "deepseek", "qwen", "grok", "gemini")

# Model makers, in precedence order, with the name fragments of their own
# models. The bare "|model" key -- Lookup's fallback for an endpoint the table
# does not know -- comes from an unprefixed upstream name, else from the
# maker's own prefix, never from a reseller's: azure_ai prices grok-4 input at
# 2.4x xAI's. Makers resell each other too (dashscope serves DeepSeek, GLM and
# Kimi), so a maker's prefix supplies only models no other maker here claims.
FIRST_PARTY = {
    "anthropic": ("claude",),
    "openai": ("gpt", "codex"),
    "gemini": ("gemini", "gemma"),
    "xai": ("grok",),
    "deepseek": ("deepseek",),
    "moonshot": ("kimi", "moonshot"),
    "zai": ("glm",),
    "mistral": ("mistral", "mixtral", "codestral", "devstral", "magistral",
                "ministral", "pixtral", "voxtral"),
    "dashscope": ("qwen", "qwq"),
}

M = 1_000_000

# The upstream field behind each of our rates. A long-context tier spells the
# same field with an "_above_<N>_tokens" suffix.
FIELDS = {
    "input": "input_cost_per_token",
    "output": "output_cost_per_token",
    "cache_read": "cache_read_input_token_cost",
    "cache_write_5m": "cache_creation_input_token_cost",
    "cache_write_1h": "cache_creation_input_token_cost_above_1hr",
}

# A tier exists where its input price does, which is how LiteLLM's own cost
# calculation finds one. The "_tokens" anchor skips the _priority, _flex and
# _batches variants, which are other service tiers, not the standard price.
TIER_KEY = re.compile(r"^input_cost_per_token_above_(\d+k?)_tokens$")


def threshold(n: str) -> int:
    """A tier's bound in tokens, from its spelling upstream: "200k" or "128000"."""
    return int(n.removesuffix("k")) * (1000 if n.endswith("k") else 1)


# LiteLLM bills these providers' tiers from the threshold itself rather than
# above it (_INCLUSIVE_THRESHOLD_PROVIDERS), so their bound is one token lower.
INCLUSIVE_THRESHOLD_PROVIDERS = {"xai"}


def is_anthropic(name: str, spec: dict) -> bool:
    return spec.get("litellm_provider") == "anthropic" or "claude" in name


def rate(name: str, spec: dict) -> dict | None:
    """Convert one LiteLLM spec into our per-million-token shape."""
    inp = spec.get(FIELDS["input"])
    out = spec.get(FIELDS["output"])
    # Missing either price, the model is left unpriced: filled in as zero,
    # its tokens would read as free.
    if inp is None or out is None:
        return None

    # A missing cache price is never a free one: Cost() would report those
    # tokens as priced at zero. Anthropic's cache writes are fixed multiples of
    # input (5m 1.25x, 1h 2x) and most Claude models read at 0.1x, so that is
    # the Claude fallback. Elsewhere a provider with no cache price bills
    # cached tokens as ordinary input, and a 1h write as a 5m one.
    anthropic = is_anthropic(name, spec)
    cache_read = spec.get(FIELDS["cache_read"])
    if cache_read is None:
        cache_read = inp * 0.1 if anthropic else inp
    cw5 = spec.get(FIELDS["cache_write_5m"])
    if cw5 is None:
        cw5 = inp * 1.25 if anthropic else inp
    cw1h = spec.get(FIELDS["cache_write_1h"])
    if cw1h is None:
        cw1h = inp * 2 if anthropic else cw5

    base = {
        "input": inp,
        "output": out,
        "cache_read": cache_read,
        "cache_write_5m": cw5,
        "cache_write_1h": cw1h,
    }
    r = {field: round(v * M, 6) for field, v in base.items()}
    tiers = long_context_tiers(spec, base)
    if tiers:
        r["tiers"] = tiers
    return r


def long_context_tiers(spec: dict, base: dict) -> list[dict]:
    """The spec's long-context tiers, ascending by threshold.

    A request whose prompt exceeds a tier's "above" is billed entirely at that
    tier's prices; Cost() in pricing.go does the selecting.
    """
    inclusive = spec.get("litellm_provider") in INCLUSIVE_THRESHOLD_PROVIDERS
    tiers = []
    for key, tier_input in spec.items():
        m = TIER_KEY.match(key)
        if m is None or tier_input is None:
            continue
        suffix = f"_above_{m.group(1)}_tokens"
        above = threshold(m.group(1))
        # A tier price upstream leaves out is the base price scaled by the
        # tier's input multiplier; output falls back to the base price.
        scale = tier_input / base["input"] if base["input"] else None
        tier = {"above": above - 1 if inclusive else above}
        for field, upstream in FIELDS.items():
            v = spec.get(upstream + suffix)
            if v is None:
                if field == "output":
                    v = base["output"]
                else:
                    v = base[field] * scale if scale is not None else tier_input
            tier[field] = round(v * M, 6)
        tiers.append(tier)
    return sorted(tiers, key=lambda t: t["above"])


def maker(model: str) -> str | None:
    """The FIRST_PARTY prefix whose models include this name, if any."""
    for prefix, fragments in FIRST_PARTY.items():
        if any(f in model for f in fragments):
            return prefix
    return None


# A regeneration with far fewer endpoint-qualified keys than the last one means
# the upstream shape moved, not that the models were withdrawn. Bare keys are
# not counted: which of them exist is decided by FIRST_PARTY, not by upstream.
MIN_KEYS_RATIO = 0.8


def qualified(rates: dict) -> int:
    return sum(1 for k in rates if not k.startswith("|"))


# Rates that must survive any regeneration, each with the upstream entry it
# comes from. A renamed upstream field silently misprices every model, so the
# values are checked, and so is the presence of the raw fields behind them: a
# fallback can equal the real price (Opus 5's 0.1x cache read is its real 0.5),
# which a value check alone cannot tell from a rename. The fields listed here
# are the ones upstream gives; "above_<N>" lists a long-context tier.
CANARIES = {
    "|claude-opus-5": ("claude-opus-5", {
        "input": 5.0, "output": 25.0, "cache_read": 0.5,
        "cache_write_5m": 6.25, "cache_write_1h": 10.0,
    }),
    # 0.05x cache reads, where the fallback would give 0.1x.
    "|claude-opus-5-5": ("claude-opus-5-5", {
        "input": 4.0, "output": 20.0, "cache_read": 0.2,
        "cache_write_5m": 5.0, "cache_write_1h": 8.0,
    }),
    # 0.025x cache reads.
    "|claude-fable-5-1": ("claude-fable-5-1", {
        "input": 10.0, "output": 50.0, "cache_read": 0.25,
        "cache_write_5m": 12.5, "cache_write_1h": 20.0,
    }),
    # Not Anthropic, so a lost cache read would fall back to the input rate.
    "|gpt-5": ("gpt-5", {"input": 1.25, "output": 10.0, "cache_read": 0.125}),
    "|claude-sonnet-4-5": ("claude-sonnet-4-5", {
        "input": 3.0, "output": 15.0, "cache_read": 0.3,
        "above_200k": {
            "input": 6.0, "output": 22.5, "cache_read": 0.6,
            "cache_write_5m": 7.5, "cache_write_1h": 12.0,
        },
    }),
    "|gpt-5.6-sol": ("gpt-5.6-sol", {
        "input": 4.0, "output": 20.0, "cache_read": 0.4,
        "above_272k": {"input": 8.0, "output": 30.0, "cache_read": 0.8},
    }),
}


def check(rates: dict, raw: dict) -> None:
    """Refuse to write a table that is obviously broken. Raises on failure."""
    if not rates:
        raise SystemExit("no rates parsed: the upstream schema has changed")

    previous = 0
    try:
        with open(PRICES) as f:
            previous = qualified(json.load(f).get("rates", {}))
    except (OSError, ValueError):
        pass
    if previous and qualified(rates) < previous * MIN_KEYS_RATIO:
        raise SystemExit(
            f"only {qualified(rates)} endpoint keys, down from {previous}: "
            "refusing to overwrite a good table with a broken one"
        )

    def expect(key: str, spec: dict, field: str, upstream: str, got: dict, want: float) -> None:
        if spec.get(upstream) is None:
            raise SystemExit(
                f"{key}: upstream field {upstream} is missing: "
                "either it was renamed or the model's pricing moved"
            )
        if abs(got.get(field, 0) - want) > 0.001:
            raise SystemExit(
                f"{key}.{field} is {got.get(field)}, expected {want}: "
                "either the rate moved or the upstream field was renamed"
            )

    for key, (name, want) in CANARIES.items():
        got, spec = rates.get(key), raw.get(name)
        if got is None or spec is None:
            raise SystemExit(f"{key} is missing: the upstream naming has changed")
        for field, value in want.items():
            if not field.startswith("above_"):
                expect(key, spec, field, FIELDS[field], got, value)
                continue
            n = field.removeprefix("above_")
            tier = next((t for t in got.get("tiers", []) if t["above"] == threshold(n)), None)
            if tier is None:
                raise SystemExit(f"{key} has no tier above {n}: the upstream tier fields have changed")
            for tf, tv in value.items():
                expect(f"{key} above {n}", spec, tf, f"{FIELDS[tf]}_above_{n}_tokens", tier, tv)


def load(argv: list[str]) -> dict:
    # Failures are loud on purpose: a table written from a broken fetch prices
    # the whole fleet as unpriced while `make prices` reports success.
    if len(argv) > 1:
        try:
            with open(argv[1]) as f:
                raw = json.load(f)
        except OSError as e:
            raise SystemExit(f"reading {argv[1]}: {e}") from e
        except ValueError as e:
            raise SystemExit(f"{argv[1]} is not JSON: {e}") from e
    else:
        try:
            with urllib.request.urlopen(SOURCE, timeout=60) as resp:
                if resp.status != 200:
                    raise SystemExit(f"{SOURCE} returned {resp.status}")
                raw = json.load(resp)
        except (urllib.error.URLError, TimeoutError) as e:
            raise SystemExit(f"fetching {SOURCE}: {e}") from e
        except ValueError as e:
            raise SystemExit(f"{SOURCE} did not return JSON: {e}") from e

    if not isinstance(raw, dict) or len(raw) < 100:
        raise SystemExit(f"upstream is {type(raw).__name__} with too few entries")
    return raw


def build(raw: dict) -> dict[str, dict]:
    rates: dict[str, dict] = {}
    first_party: dict[str, tuple[int, dict]] = {}
    precedence = list(FIRST_PARTY)
    for name, spec in raw.items():
        if not isinstance(spec, dict) or spec.get("mode") not in (None, "chat", "responses"):
            continue
        provider = spec.get("litellm_provider", "")
        if provider not in KEEP_PROVIDERS and not any(s in name for s in KEEP_SUBSTRINGS):
            continue
        r = rate(name, spec)
        if r is None or (r["input"] == 0 and r["output"] == 0):
            continue

        # Keys are "endpoint|model". LiteLLM prefixes some names with their
        # provider ("moonshot/kimi-k3"); that goes in our endpoint slot so a
        # harness reporting a provider resolves precisely.
        endpoint, model = name.lower().split("/", 1) if "/" in name else ("", name.lower())
        rates[f"{endpoint}|{model}"] = r
        if endpoint in FIRST_PARTY and maker(model) in (None, endpoint):
            rank = precedence.index(endpoint)
            if model not in first_party or rank < first_party[model][0]:
                first_party[model] = (rank, r)

    # An unprefixed upstream name is its own bare key and outranks a maker's.
    for model, (_, r) in first_party.items():
        rates.setdefault(f"|{model}", r)
    return rates


def main() -> int:
    raw = load(sys.argv)
    rates = build(raw)
    check(rates, raw)

    out = {
        "version": f"litellm:{len(rates)}",
        "source": SOURCE,
        "rates": rates,
    }
    with open(PRICES, "w") as f:
        json.dump(out, f, indent=1, sort_keys=True)
        f.write("\n")
    print(f"wrote {PRICES}: {len(rates)} keys")
    return 0


if __name__ == "__main__":
    sys.exit(main())
