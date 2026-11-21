# 技术说明：可追溯的响度整备链路

## 1. 处理流程

```text
WAV 严格读取
  -> v1/v2/v3 严格解析
  -> 统一 v3 内部表示
  -> 边界推导或显式边界校验
  -> 逐段短期响度峰值 / 真峰值 / 返工判定
  -> 按段临时文件 + 原子重命名
  -> 原子更新哈希链清单
  -> 强制终止后的增量恢复
  -> 完整快照发布与不可变查询
```

所有持久化 JSON 都使用 UTF-8、排序键和紧凑分隔符生成规范字节。轨迹日志不记录墙钟时间或绝对路径，写出前按素材、提交序号、段号和事件类型稳定排序，因此同一随机种子和同一操作序列得到逐字节一致的日志。

## 2. 解析与内部表示

`MetadataDocument` 是统一表示。v1 只有整轨字段，段列表为空；v2 的段只有声明值，没有边界；v3 的段有显式 `start_sample`/`end_sample`，并要求从 0 连续覆盖到 `duration_samples`。

严格错误由 `AppError` 携带 `code` 和 `field`：

| 场景 | 错误码 | 首个字段示例 |
| --- | --- | --- |
| 元数据缺字段 | `E_META_INVALID` | `asset_id` |
| 版本未知 | `E_META_INVALID` | `version` |
| v3 校验码不符 | `E_META_INVALID` | `archive_checksum` |
| v3 边界断裂 | `E_META_INVALID` | `segments[1].start_sample` |
| WAV 掩码非法 | `E_WAV_INVALID` | `fmt.dwChannelMask` |
| 版本降级有损 | `E_VERSION_LOSS` | 损失清单含段号和字段 |
| 跨版本不一致 | `E_SEGMENT_MISMATCH` | `first_mismatch.segment_index/field` |
| 同素材重复任务 | `E_BUSY` | `asset_id` |
| 超内存预算 | `E_MEMORY_BUDGET` | `memory_bytes` |
| 链断或段文件截断 | `E_RECOVERY_INVALID` | `segments[i].chain_hash` 或 `.file` |
| 发布版本过期 | `E_VERSION_STALE` | `content_hash` / `manifest_chain_head` |

v3 归档校验码算法固定为：

```text
sha256(canonical-json:v1, excluding archive_checksum)
```

## 3. 边界规则

- v3：直接采用元数据边界，不允许推断或静默修补。
- v2：`start_i=floor(i*duration/count)`，`end_i=floor((i+1)*duration/count)`。
- v1：按配置 `default_segment_samples` 固定窗推导，最后一段截断到整轨末尾。

升级时，映射表为每个决策记录 `decision_id`、目标字段、动作、来源、算法、输入和结果。v2 原声明值保留在 `previous_declared` 中，并明确记录“v2 无显式边界，已在新边界上重算”。v1 没有真峰值上限，升级时只能采用显式配置策略 `v1_true_peak_limit_dbtp`，映射表将该动作标为 `policy`，不是解析默认值。

降级默认抛出 `E_VERSION_LOSS`。`loss_manifest` 同时包含：

- 每个受影响段号；
- 无法表达的逐段字段；
- 无法表达的全局字段；
- 是否可逆；
- 可独立复现的字段映射表。

调用方显式传入 `allow_loss=True` 后才生成目标版本载荷。

## 4. 响度与真峰值

短期响度峰值采用 BS.1770-4 的 48 kHz K 加权系数。每声道通过 FFT 频率响应实现 K 加权，按下混权重求逐帧能量：

```text
E[n] = sum_c(weight_c * K(x_c)[n]^2)
LUFS = -0.691 + 10*log10(mean(E over window))
```

段内短期窗口取配置的 `short_term_window_samples`（默认 3 秒）和段长中的较小值，按 `short_term_hop_samples` 滑动，取最大 LUFS。短于 3 秒的段使用整段。

真峰值对每声道 PCM 做 4 倍 Catmull-Rom 插值，再取所有声道绝对最大值：

```text
true_peak_dbtp = 20*log10(max(abs(x) and abs(interpolated(x))))
```

该实现确定、无需外部 DSP 库，并在报告的 `reproduction` 中记录算法版本、容差、每段边界、声明值、重算值和依据字段。

## 5. 并发与内存

`TaskScheduler` 只从配置文件读取 `max_parallel`、`per_task_memory_bytes` 和 `global_memory_bytes`，没有运行时提高预算的 CLI 参数。

- 同一素材在排队或运行期间再次提交返回 `E_BUSY`。
- 单任务超过配置预算立即返回 `E_MEMORY_BUDGET`。
- 可运行任务按提交序号稳定选择。
- 累计内存不足时任务保持等待，并记录 `task_wait_memory`。
- 单个素材内部的分段 future 严格按段号提交，并按段号顺序取结果和提交。

归档服务另有进程级 `flock`，防止两个进程同时整备同一素材。

## 6. 原子提交与恢复

清单目录：

```text
workspace/
  manifests/<asset_id>.json
  segments/<asset_id>/seg-000000.json
  versions/ver-000001.json
  versions/index.json
  trace.jsonl
```

每段提交顺序：

1. 写段临时文件并 `fsync`；
2. `os.replace` 原子命名到最终段文件；
3. 追加清单条目，计算 `content_sha256` 链；
4. 写清单临时文件并 `fsync`；
5. `os.replace` 原子替换清单；
6. 记录轨迹。

链哈希：

```text
sha256(prev_chain + "\n" + content_sha256 + "\n" + parameters_version + "\n" + index)
```

恢复只信任磁盘清单与已提交段文件：

- 清单缺失：创建零段清单。
- 已提交段：校验文件存在、JSON 完整、内容哈希、前后链指针，然后跳过，不重写文件。
- 未提交段：重新计算并原子提交。
- 清单外残留段文件：记录为 orphan，不作为已提交版本。
- 清单自身有校验码；段文件截断、内容哈希不符或链断时，按段号升序报告首个异常。

## 7. 发布隔离

发布快照包含段结论、返工判定、清单链头、字段映射和损失清单。文件发布分两步：

1. 在独立结构中构建并校验完整快照，写不可变版本文件；
2. 最后原子替换 `versions/index.json` 指针。

查询先读取索引，再读取不可变版本文件，并再次验证内容哈希。当前版本查询还会校验归档清单链头。版本哈希不匹配或清单链头已变化返回 `E_VERSION_STALE`，不会返回半更新段结论。进程内 `InMemoryVersionPublisher` 用引用整体替换，读者要么看到完整旧版本，要么看到完整新版本。

## 8. 确定性样本

默认样本组合：

| 样本 | 位深 | 布局 | 段数 |
| --- | --- | --- | --- |
| `mono-16` | 16-bit | 单声道 | 5 |
| `surround-24` | 24-bit | 5.1 | 5 |
| `mono-24` | 24-bit | 单声道 | 5 |
| `surround-16` | 16-bit | 5.1 | 5 |

每个 WAV 同时生成 v1、v2、v3 元数据。v2 使用 4 个等分段的派生边界，并故意将一个声明超限标记翻转，供复核报告展示“声明值与重算值不一致”的原因。生成器还自动执行“提交 2 段后抛出模拟强杀 -> 恢复剩余 3 段 -> 发布 -> 按哈希查询”的演示。

