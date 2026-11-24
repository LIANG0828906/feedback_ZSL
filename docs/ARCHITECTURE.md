# 架构与确定性设计

## 数据流

```text
manifest.json + truth.json + frames.hdrseq
        │
        ├─ 声明/真值校验 ── E_META
        ├─ 容器/CRC/数值校验 ── E_DECODE
        ▼
decoded linear frames
        │
        ├─ exposure
        ├─ source primaries → BT.709 linear matrix
        ├─ gamut desaturate / clip
        ├─ ACES / Reinhard
        ├─ sRGB 8-bit encode
        ▼
SDR pixels + proof record
        │
        ├─ worker bounded frame buffer
        ├─ serialized SQLite segment transaction
        ▼
segments + frames + snapshot_meta
        │
        ├─ compare: pixels/proof first difference
        └─ audit: local stats/truth/proof completeness
```

## 样本生成

生成器使用固定种子和固定路由描述。色块目标值先在 BT.709 目标线性空间构造，再通过 ACES 逆函数与源色域逆矩阵反解出源线性 RGB，因此真值不是转换链回填，而是生成阶段构造并写入 `truth.json`。

`rgb10le` 使用 uint16 小端存储 0..1023 的 10 位值；`linear_float32_le` 使用 float32 小端存储线性光值。

`HDRSEQ01` 容器：

```text
32-byte file header:
  magic "HDRSEQ01", version, width, height, channels,
  bytes_per_channel, frame_count

per frame:
  magic "FRM1", uint64 timestamp_ms, uint32 payload_len,
  payload, uint32 IEEE CRC32(payload)
```

## 转换链

1. 输入样本归一化为源色域线性 RGB。
2. 乘以 `2^exposure_ev`。
3. 通过声明 primaries 的 3×3 矩阵转换到 BT.709 线性 RGB。
4. `desaturate` 将越界颜色沿亮度方向收敛到 `[0,1]`；`clip` 直接截断。
5. 每通道使用 ACES 拟合曲线或 Reinhard 曲线。
6. 使用标准 sRGB 传递函数编码为 RGB8。

proof 的输出统计、色块实际线性 RGB 与偏差都从量化后的 SDR 像素重新解码计算，保证第三方可按同一数据复核。

## proof 记录

字段包括：

- `record_version`、`algorithm_version`、`param_version`；
- 路 ID、帧号、分段号、时间戳；
- 输入声明、源帧 SHA-256；
- 完整转换参数、转换链摘要；
- 尺寸、通道、逐通道 min/max/mean 线性值、输出峰值 nits、像素 SHA-256；
- 每个色块的 expected/actual/delta/delta_max；
- 样本真值指纹与 `record_sha256`。

`record_sha256` 计算时先清零自身字段，对 compact JSON 做 SHA-256 后写回，因此安全地自封存且可重复验证。

## 状态机

路：

```text
pending → running → completed
                 ↘ failed → running
pending/running/failed → cancelled → running
```

分段：

```text
pending → running → committed
                 ↘ failed → running
```

`committed` 为终态，不能回到 `running`。非法迁移返回 `E_STATE`，迁移前状态不变。

参数更新计划以 `(route_id, from_segment, params)` 外部描述。低于 `from_segment` 的已提交段保留旧参数版本，未提交段使用新参数版本。快照整体参数计划版本由 base 参数与全部 update 规范化哈希得到；无更新时直接等于 base 参数的规范化哈希。重启时若计划不一致，整库作废。

## 原子快照与整体版本标识

SQLite 使用 `modernc.org/sqlite`，单连接、写互斥、WAL、`synchronous=FULL`。

每个分段在一个事务中写入：

- 该段全部输出帧（像素、pixel hash、proof JSON、record hash）；
- 分段 source/output/proof/content hash；
- 参数/算法/真值版本、帧数、proof summary。

快照元信息包含：

```text
schema_version
algorithm_version
param_version          # 完整参数计划版本
truth_fingerprint
```

加载时验证路由声明、分段计划、提交顺序、每段参数版本、每帧参数/算法/真值版本、pixel/proof/record hash、分段摘要和内容 hash。任一失败即用单事务清空所有持久化分段并全量重算，禁止混用。

## 并发与资源边界

- 工作池最大并行度默认 4，硬上限 16。
- 每个路 worker 有独立帧缓冲，默认容量 2。
- 生产者填满缓冲时阻塞排队，发出 `frame_buffer_blocked` 事件，`reason=frame_buffer_full`。
- 所有 worker 共用同一 SQLite Store；写锁与单连接保证提交不交错。
- `commit_order` 按路序与段号固定，不依赖 goroutine 完成顺序。

## 错误注入

注入文件只包含外部描述：

```json
{
  "meta_conflicts": [],
  "bad_frames": [],
  "parameter_injections": [],
  "segment_failures": []
}
```

引擎在对应路/段读取，不修改实现代码。分段内部失败为一次性注入：显示 `E_SEG` 后可将该段置 failed，再合法迁移到 running 并重试；其他路与段继续执行。

## 独立审计

审计只使用：

- SQLite 中的 proof、像素、分段摘要和元信息；
- `manifest.json`、`truth.json` 与真值指纹；
- 本包内独立实现的 sRGB 反解码与统计公式。

审计不导入 `internal/convert`。它检查记录唯一性、完整性、proof 自哈希、身份字段、版本、像素 hash、统计重算、色块 actual 重算、真值偏差与分段摘要。

## 一致性比对

比对逐路、逐帧先比较像素 bytes，再比较 proof JSON。像素不同定位到首个 byte；proof 不同递归定位首个 JSON 字段路径。错误返回 `E_DIFF` 并携带路、帧、字段和分段。
