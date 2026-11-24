# 内容完整性核验内核实现说明

## 1. 代码结构

- `cmd/diagnose`：Windows/Linux 通用 CLI 与诊断界面入口，子命令为 `gen`、`verify`、`crosscheck`、`review`、`serve`。
- `internal/core`：错误码、清单、参数、证据结构、报告和稳定 JSON/哈希工具。
- `internal/samplegen`：固定种子确定性生成 192×108 RGBA PNG、原始基准帧、真值清单和真值指纹。
- `internal/loader`：样本清单/PNG 加载与结构校验。
- `internal/watermark`：16×3 比特格点水印绘制、3×3 平均采样、置信度、缺失/错位搜索。
- `internal/forensics`：亮度归一化残差、局部块峰值能量、边缘掩膜噪声方差、时序块 Z 分数、Sobel 方向一致性和判决。
- `internal/engine`：证据 DAG、缓存键、精确失效、worker 调度、资源槽、失败重试和报告生成。
- `internal/checkpoint`：`modernc.org/sqlite` 纯 Go SQLite 存储、批次事务、提交日志和快照版本校验。
- `internal/pipeline`：实时增量、断点恢复、一次性全量三条路径的参数剧本和自动逐项比较。
- `internal/review`：只读取 SQLite 当前证据与 `truth.fingerprint.json` 的独立复核器，不导入核验引擎/取证包。
- `internal/web`：嵌入的无 CDN 本地诊断页面和 JSON API。

## 2. 错误契约

所有核心错误均序列化为 `core.Error`：

- `E_DECODE`：清单缺失/损坏、PNG 截断等解码失败。
- `E_META`：宽高、通道数、帧数、时间戳等元数据不一致；包含 `frame` 与 `field`。
- `E_MARK`：水印区域越界或修正区域越界；字段为 `watermark_region`。
- `E_PARA`：分块尺寸、阈值、并发/缓冲参数越界；包含字段名。
- `E_DIFF`：增量/恢复/全量或独立复核不一致；可包含 `frame`、`evidence_type`、`block_x`、`block_y`、`field`。
- `E_TXN`：检查点批次提交中断；`batch` 指出首个未完成批次。
- `E_RESOURCE`：单证据所需像素缓冲超过每 worker 上限。

## 3. 样本与真值

默认样本位于 `samples/default`，共 24 帧：

- 渐变背景、矩形/圆/三角形/横条图元、16-bit 帧号水印、全局亮度扰动和轻微颗粒。
- `frames/frame_XXXX.png` 是导出/传输后的实际帧。
- `baseline/frame_XXXX.png` 是替换前原始基准像素。
- `truth.json` 包含水印矩形、图元外接框、时间戳、基准哈希和替换区间掩码。
- `truth.fingerprint.json` 包含清单/真值哈希与声明替换区间，供独立复核使用。
- 默认替换区间为第 13–16 帧；这些帧的水印区和局部画面被来自第 5 帧的对齐拼接块覆盖，并带外来颗粒特征。

重新生成：

```bash
go run ./cmd/diagnose gen -sample samples/default
```

## 4. 证据依赖与缓存键

显式 DAG 为：

```text
watermark ─┐
residual  ─┼─> verdict
noise     ─┘
```

`internal/engine/graph.go` 暴露 `Dependencies`、`Dependents` 和稳定拓扑序。规范提交顺序为帧号升序，每帧依次 `watermark`、`residual`、`noise`、`verdict`。

缓存键输入为：

```text
sha256({
  algorithm_version,
  content_hash,
  evidence_type,
  region?,
  params: 仅该证据依赖的参数子集,
  context_hashes?: 残差/噪声引用的前序帧哈希,
  upstream_keys?: verdict 的三类上游键
})
```

因此：

- 修改 `block_size`：只失效全部 `noise` 与下游 `verdict`。
- 修改 `verdict_threshold`：只失效全部 `verdict`。
- 修正水印区域：只失效全部 `watermark` 与下游 `verdict`。
- 修改 `max_parallel`、批次大小等调度参数：不改变证据值，不失效证据。

失效清单结构为 `{frame,type,reason}`，原因形如 `param:block_size` 或 `upstream:noise`。

## 5. 检测规则

- 水印：按列聚合多行格点亮度，输出每个采样点的 bit、亮度和置信度；在 `search_radius` 内选择置信度平台上距声明区域最近的对齐位置。低对比/低置信度判为缺失，偏移超过 `alignment_tolerance` 判为错位。
- 时间轴：相邻解码值不为 `prev+1` 时记录 `decoded_jump`，缺失/错位分别记录 `watermark_missing`、`watermark_misaligned`。
- 残差：做帧均值亮度归一化，屏蔽必然逐帧变化的声明水印区，计算滞后能量、MAE 和 16 像素局部块能量的 90 分位。
- 噪声：3×3 高通残差上，用 Sobel 梯度低于阈值的平坦像素估计块方差；同时计算跨块对数稳健 Z 分数和同坐标前序块时序 Z 分数。
- 边缘：Sobel 方向按 π 折算到 `edge_bins`，以主方向权重作为方向一致性，并输出每个真值图元外接框的统计。
- 判决：加权水印、残差、噪声分数；默认要求总分达到阈值且至少两类证据异常，体现水印断裂与噪声异常互证。

## 6. SQLite 检查点

数据库表：

- `meta`：根内容哈希、全局参数版本、算法版本、完整参数 JSON。
- `batches`：单调批次号和 `pending/committed` 状态。
- `evidence`：按 `cache_key` 保存证据 JSON、内容哈希、参数哈希、算法版本和摘要。
- `evidence_log`：只追加的稳定提交序列。
- `current_evidence`：帧/证据类型到当前缓存键的原子指针。

写入由 `checkpoint.Store` 的互斥锁和 `SetMaxOpenConns(1)` 串行化；每个证据批在一个 SQL 事务中写证据、日志、当前指针、批次状态和元数据。新参数产生新 `cache_key`，不会静默覆盖旧键；当前指针在事务内原子切换。

重启时若根内容哈希、参数版本或算法版本不匹配，会清空检查点表并从空缓存全量重算。未完成的 `pending` 批次通过 `E_TXN` 和首个批次号报告，随后以同批次号幂等重试。

## 7. 三路一致与独立复核

`pipeline.RunCrossCheck` 执行同一参数剧本：

1. 初始水印区域故意偏移 3 像素。
2. 修正水印区域。
3. 将 `block_size` 从 16 改为 12。
4. 将 `verdict_threshold` 从 0.50 改为 0.65。

恢复路径在第 2 个证据批提交前注入事务错误，关闭 SQLite，再重新打开并报告首个未完成批次 2，随后恢复。每个参数状态都与从空缓存运行的全量报告比较：

- 逐帧四类证据；
- 逐块噪声统计；
- 时间轴和替换区间；
- 当前证据的规范提交序列、缓存键和摘要。

独立复核器 `internal/review` 从 `current_evidence + evidence` 读取持久化 JSON，校验摘要，用自带的加权/区间规则重算，并对照真值指纹中的区间。

## 8. 并发与资源

- `max_parallel` 限制 worker 槽。
- 每个证据估算约 `8 × width × height` 个 float 缓冲；超过 `max_pixels_per_worker` 时该证据返回 `E_RESOURCE` 并记为失败。
- 槽位满时任务排队，同时向报告写入 `pixel_buffer_exhausted_waiting_in_queue` 事件，不丢弃证据。
- 单个证据 panic/错误会记录为 `FailureRecord`；其他帧/独立证据继续，修正故障后再次 `Run` 即重试缺失节点。

## 9. 本地界面

```bash
go run ./cmd/diagnose serve -sample samples/default -artifacts artifacts -addr 127.0.0.1:8088
```

浏览器打开输出的本地地址。页面提供：

- 帧时间轴和 W/R/N/V 状态矩阵；
- 水印解码曲线与断裂原因；
- 选中帧、基准帧切换和证据明细；
- 噪声方差热力图及块坐标/Z 分数；
- 真值/预测替换区间；
- 块尺寸、阈值、水印区域调整；
- 三路比对和独立复核 JSON 结果。

所有静态资源由 Go `embed` 打包，不访问 CDN、在线账号或付费服务。
