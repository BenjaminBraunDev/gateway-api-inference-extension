# Prefix Cache Scorer Plugin

This plugin scores candidate endpoints by estimating **prompt prefix cache reuse** on each model server.

It is registered as type `prefix-cache-scorer` and runs as a scheduling scorer.

## What it does

For each incoming request, the plugin:

1. Extracts user input from one of the supported API shapes:
   - Completions
   - Chat Completions
   - Conversations
   - Responses
2. Splits input into fixed-size blocks (in tokens, approximated to characters).
3. Builds a rolling hash chain across blocks (including model name and optional `cache_salt`).
4. Looks up which pods are likely to already have each prefix block cached.
5. Computes per-endpoint score

Higher score means more expected prefix-cache hits and lower prefill work.

## How cache state is learned

After scheduling, the plugin records selected endpoint(s) into an in-memory index:

- Primary selected endpoint is always updated.
- If a `prefill` profile is present (P/D disaggregation), its endpoint is also updated.

The index is per-pod LRU plus a reverse map from block hash to pods.

## Configuration

The plugin config supports:

- `autoTune` (default true)
  - If true, block size and per-pod capacity can be inferred from endpoint metrics.
- `blockSizeTokens`
  - Prefix block size in tokens. Should reflect the underlying model server's cache chunk size (vLLM default: 16).
  - Minimum recommended value: **16**. Lower values cause the per-pod LRU indexer to consume excessive memory (~60–70 bytes per entry × `lruCapacityPerServer` × number of pods) and can OOM the EPP under load.
  - When `autoTune` is enabled, the effective block size is clamped to a floor of 16 even if the model server reports a smaller value. Manually configured values below 16 are honored but trigger a startup warning.
- `maxPrefixBlocksToMatch`
  - Caps how much of a long prompt is considered.
- `lruCapacityPerServer`
  - Default per-pod index capacity when endpoint metrics are unavailable.
- `blockSize`
  - Deprecated legacy field (characters). Do not use.

## Operational notes

- Prefix matching is approximate and intentionally lightweight.
- Matching is model-scoped (same prompt across different models does not collide).
- Pods no longer active are periodically removed from the index.
- Hashing uses token-to-character approximation, so it is a heuristic, not exact tokenizer parity.
