# 损坏分片样本

`corrupt_audio_fragment.json` 是 `g01_neon_city` 音频样本 base64 片段的中位字节翻转副本，字段：

- `raw_bytes`：已损坏的 base64 字符片段；
- `offset`：第一次翻转的字符位置。

完整的四类可执行异常通过 `samples/injections/*.json` 参数注入：

- `conflict.json` -> `E_CONFLICT`
- `order.json` -> `E_ORDER`
- `missing_track.json` -> `E_TRACK`
- `corrupt_segment.json` -> 导入阶段 `E_CORRUPT`，分段封装阶段 `E_CORRUPT` 且仅目标段失败
