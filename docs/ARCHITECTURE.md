# 架构与一致性设计

## 转换链

共享链由源图集头部和 `TransformConfig` 决定：

```text
source transfer decode
  -> source RGB to XYZ
  -> Bradford white adaptation
  -> XYZ to target RGB
  -> exposure (2^EV)
  -> source_peak / target_peak scaling
  -> tone curve
  -> target transfer encode
  -> bit-depth quantization
  -> float16 storage
```

目标色彩空间、目标传递函数、目标峰值、源/目标白点和适应方法共同组成
`shared_key`。任一字段改变，所有区块都进入受影响集合。只改变某区块的
`exposure_ev`、`tone_curve` 或 `tone_strength` 时，仅该区块重算。

转换使用固定 float64 运算顺序。矩阵在 15 位小数处取整以稳定摘要；
量化使用 `floor(x * max_code + 0.5)`；JSON 使用排序键和紧凑分隔符。
导出和图集头不写入时间戳或随机值。

## 区块缓存

每个缓存键是规范 JSON 的 SHA-256：

```text
atlas.fingerprint
+ tile index
+ ColorChain digest
+ bit_depth
+ TileParams
```

`ColorChain digest` 已包含源色彩空间、源传递函数、源峰值和全部共享配置。
每个缓存项保存：

- float16 区块载荷；
- 载荷 SHA-256；
- 最大/平均/RMSE/P99 码值偏差；
- 裁切像素比例；
- 误差摘要哈希。

版本只引用哈希，不修改缓存项。全量复算检查会重新执行所有区块，并比较
载荷哈希和误差摘要在声明容差内是否一致。

## 提交事务

`AtlasPipeline` 维护两把锁：

- `_submission_lock`：每个图集最多一个转换事务，重复提交返回 `E_BUSY`；
- `_publish_lock`：保护版本元组、缓存映射和当前版本指针。

提交流程：

1. 校验期望父版本、配置、区块参数表和峰值/曲线组合。
2. 创建 `ConversionJob`，计算每个区块缓存键，复用未受影响缓存。
3. 逐区块计算到事务私有缓存；期间可取消或写检查点。
4. 全部完成后构建不可变 `Version`。
5. 在同一发布锁内同时替换 `_cache`、`_versions` 和 `_current_version`。
6. 释放单任务锁。

任何异常、拒绝或取消都不会进入第 5 步，因此旧版本、旧预览和旧哈希清单保持
完整。预览在发布锁内读取版本和全部区块缓存，返回自洽快照。

## 版本与哈希链

版本字段包括：

- 单调递增版本号和父版本；
- 配置与完整区块参数表；
- 受影响区块、复用区块；
- 每区块输出哈希；
- 输出内容哈希；
- 转换指纹、链路中间摘要和误差摘要哈希；
- 父版本链路哈希和本版本链路哈希。

计算顺序：

```text
content_hash = H([{"tile": i, "hash": tile_hash_i}, ...])
chain_digests.tile_content = content_hash
chain_hash = H(canonical(version_without_chain_hash))
```

`chain_hash` 的输入包含 `parent_chain_hash`，因此任何历史字段或链路改写都会
使后续链断裂。`diff()` 比较两个版本的共享配置和逐区块参数。

`rollback(n)` 不删除或改写版本，而是追加一个新版本：参数和输出引用历史版本
`n`，`rollback_of=n`，父版本是当前最新版本。后续提交以新回退版本为父版本。

## 快照与恢复

单文件 JSON 快照包含：

- 压缩后的原始图集字节与 SHA-256；
- 图集指纹；
- 完整版本记录；
- 所有版本引用的区块缓存；
- 当前版本和预览状态；
- 确定性轨迹日志；
- 可选未完成事务：目标配置、参数、受影响集合、已完成区块和缓存键。

保存使用同目录临时文件，写入后 `fsync`，再 `os.replace` 原子重命名。
GUI 与设置 `--snapshot-out` 的 CLI 会启用自动检查点：事务创建时、每个区块
完成后和发布后都会重写同一快照；取消/失败则写入不含 pending 事务的完整旧状态。
所有快照写入另有一把进程内锁，避免较旧进度覆盖较新进度。
恢复验证：

1. 嵌入图集哈希和格式；
2. 版本字段完整性；
3. 版本号连续性、父指针和 `parent_chain_hash`；
4. 内容哈希、转换指纹、误差摘要和链路摘要；
5. 每个区块缓存长度、形状、哈希和摘要；
6. 快照当前版本和预览版本范围。

断链或字段缺失返回 `E_RECOVERY_INVALID`。未完成事务只重算缺失受影响区块，
已完成区块直接复用；恢复后一次性发布目标版本。

## 导出

导出在发布锁内捕获期望版本及区块集合，校验：

- 期望版本等于当前版本；
- 期望内容哈希；
- 期望区块哈希清单逐项一致。

不匹配返回 `E_VERSION_STALE`。导出图集头写入 `pipeline_version`；JSON 报告
记录版本、配置、区块参数、每区块误差、区块哈希、内容哈希、链路哈希和报告
自身哈希。`verify_export()` 重新解析导出图集，按导出网格重新分区，复算每个
区块哈希和整体内容哈希，并比对图集头版本与报告版本。
