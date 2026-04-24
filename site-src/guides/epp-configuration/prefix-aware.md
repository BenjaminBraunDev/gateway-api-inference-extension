# Prefix Cache Aware Plugin Configuration

The [prefix cache plugin](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/7617439188b410670ed0f1ff805a3b7f9918a75b/pkg/epp/scheduling/framework/plugins/multi/prefix/plugin.go#L63)
takes advantage of the prefix caching (e.g., [vllm APC](https://docs.vllm.ai/en/latest/features/automatic_prefix_caching.html))
feature of model servers, and optimizes request scheduling by placing requests sharing the longest
prefixes to the same server as much as possible, while balancing the server load by considering kv-cache
and queue depth.

## Enable the prefix cache plugin

Like any other plugins, the prefix cache aware plugin can be enabled/disabled via the [plugin config file](config-text.md), and is enabled in the [default configuration](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/config/charts/inferencepool/templates/epp-config.yaml).

## Customize the prefix cache plugin

The prefix cache plugin exposes the following advanced configuration parameters:

* `blockSizeTokens`: The plugin matches prefixes in the unit of blocks. This is the size
of each block in tokens, and should reflect the underlying model server's cache chunk size.
When `autoTune` is enabled, EPP dynamically fetches this from the inference engine metrics
and only falls back to the configured value when the metric is unavailable. In vLLM, the
metric name is `vllm:cache_config_info` and the metric label is `block_size`. See the
[model server protocol](https://github.com/kubernetes-sigs/gateway-api-inference-extension/tree/main/docs/proposals/003-model-server-protocol)
for more details.

    The default is **16 tokens**, which matches vLLM's default block size and is recommended
    for most deployments.

    > Note on memory usage:
        Each LRU indexer entry costs ~60–70 bytes of EPP memory, and the indexer holds
        `lruCapacityPerServer × num_pods` entries. Setting `blockSizeTokens` below **16**
        scales the indexer state to potentially gigabytes and can OOM the EPP under load.
        For this reason:

        * When `autoTune` is enabled, the effective block size is clamped to a floor of 16
          even if the model server reports a smaller value.
        * Manually configured values below 16 are honored but trigger a startup warning.

* `blockSize` (deprecated): Legacy block size defined in number of characters. Use
`blockSizeTokens` instead.

* `maxPrefixBlocksToMatch`: The maximum number of blocks to find prefix match. The default is
256 (roughly 4096 tokens at the default block size). This is useful to tradeoff prefix match accuracy
for performance.

* `lruCapacityPerServer`: Maximum capacity of the prefix LRU cache in number of block hashes per
server (pod). Similar to `blockSizeTokens`, EPP can dynamically fetch this from the inference engine
metrics endpoints when `autoTune` is enabled. In vLLM, the metric name is `vllm:cache_config_info`
and the metric label is `num_gpu_blocks`. See the
[model server protocol](https://github.com/kubernetes-sigs/gateway-api-inference-extension/tree/main/docs/proposals/003-model-server-protocol)
for more details.

    If such metric is not available, you can follow the guide below on how to estimate this.

        The prefix cache plugin estimates the prefix cache indexes in model server HBMs.  In the perfect
        scenario, EPP has the exact same prefix cache entries per model server as their HBM cache entries. If
        the EPP cache is smaller than HBM cache, a positive EPP cache match is more accurate, but there are more
        false cache misses. If the EPP cache is larger than the HBM cache, then there are more false cache hits.
        Therefore **the EPP prefix cache indexer size should be as close as possible to the HBM cache size.**

        Below are the formulas to estimate the EPP prefix indexer size:

        ```
        max_kv_tokens_per_server = (HBM_size - model_size) / kv_size_per_token
        lru_indexer_capacity_per_server = max_kv_tokens_per_server / blockSizeTokens
        ```

        Let's take an example:

        * Model: qwen3 32B
        * Accelerator: Nvidia H100 80GB
        * Num replicas: 3

        ```
        max_kv_tokens_per_server = (80GB - 16GB) / 128KB = 500,000
        # assume blockSizeTokens = 16 (default, matches vLLM)
        lru_indexer_capacity_per_server = 500,000 / 16 = 31,250
        ```
