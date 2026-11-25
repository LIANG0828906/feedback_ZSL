# maskpreview

面向游戏 UI 图标的确定性掩码分割预览内核。项目包含样本生成、解析真值、区域生长、形态学清理、区域几何统计、交互预览/精确重算双路径、分块状态机、SQLite 检查点、断点恢复、独立验证器和端到端演示。

实现使用 Go 1.22+ 与纯 Go SQLite 驱动 `modernc.org/sqlite`，不依赖 CGO、Docker 或外部素材。样本生成完成后，实验只在本地 PNG、JSON 与 SQLite 文件上运行。

## 快速开始

Windows PowerShell：

```powershell
go mod download
go run .\cmd\maskdemo -samples .\samples -artifacts .\artifacts
go test .\...
go test -race .\...
go run .\cmd\verify -manifest-dir .\samples -db .\artifacts\full.db
go run .\cmd\verify -manifest-dir .\samples -db .\artifacts\incremental.db
```

Linux/macOS：

```bash
go mod download
go run ./cmd/maskdemo -samples ./samples -artifacts ./artifacts
go test ./...
go test -race ./...
go run ./cmd/verify -manifest-dir ./samples -db ./artifacts/full.db
go run ./cmd/verify -manifest-dir ./samples -db ./artifacts/incremental.db
```

`go mod download` 只在首次拉取依赖时需要网络；生成器、样本、测试和检查点恢复均离线运行。已用 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build` 验证可交叉编译 Windows 可执行文件。

## 交付结构

| 路径 | 作用 |
| --- | --- |
| `internal/core` | 错误码、参数区间、真值清单、版本快照与哈希 |
| `internal/sample` | 固定种子图标/参考掩码生成与加载校验 |
| `internal/segment` | 量化、区域生长、清理、几何/轮廓、最优匹配、预览 |
| `internal/engine` | 块状态机、预算调度、SQLite 检查点、三路径执行 |
| `internal/verifier` | 只读阶段产物重算掩码的独立验证器 |
| `internal/demo` | 端到端演示、异常探针、篡改恢复与报告 |
| `cmd/maskdemo` | 本地演示入口 |
| `cmd/verify` | 独立验证 CLI |
| `samples/` | 生成的 PNG 图标、像素真值掩码、`manifest.json` |
| `artifacts/` | 全量/增量 SQLite、篡改恢复库、`run-report.json` |

## 样本与真值

默认种子 `424242`，生成 `64x64`、`96x96`、`144x144` 三张 RGBA 图标。每张图包含：

- 两张高对比色块目标（圆形、菱形）；
- 一张与局部背景仅约 31–33 级/通道差的低对比目标；
- 水平/垂直双方向渐变背景；
- 逐像素确定性嵌入噪声。

`manifest.json` 中每个样本记录：宽高、通道数、PNG/参考掩码路径、图像 SHA-256、区域数量、每个区域的面积、像素中心重心、外接框、种子点、量化规则、默认参数、声明预览延迟和预览容差。`*_truth.png` 是解析构造的像素级标签掩码（0 背景，1..N 区域）。

真值不读取任何分割结果。加载时：

- 图像哈希、PNG 解码失败返回 `E_DECODE`；
- 宽、高、通道或真值声明不一致返回 `E_META`，错误携带字段名及 `got/want` 两侧值；
- 参考掩码区域数与区域面积也会与清单交叉校验。

## 分割链

1. 按快照中的 6-bit 量化规则量化 RGB。
2. 从每个种子点取 3x3 邻域参考色。
3. 确定性 BFS 区域生长：候选像素同时满足种子调色板颜色距离上限，且与当前像素的最大通道差分不超过梯度阈值。
4. 二值阈值化。
5. 以结构元素尺寸做形态学开重建、闭重建：开重建移除小于结构元素的孤立噪声并保留有效目标原始边界，闭重建填洞。
6. 4 邻域连通域统计面积、像素中心重心、闭区间外接框和闭合外轮廓。

参数区间固定并可机器读取：

| 参数 | 合法区间 |
| --- | --- |
| `color_tolerance` | `[0,255]` |
| `gradient_threshold` | `[0,255]` |
| `kernel_size` | `[3,15]` 且必须为奇数 |
| `growth_iterations` | `[1,100000]` |

越界返回 `E_PARA`，含字段名、实测值和合法区间。分割是单次确定性扫描；块并行只影响提交前计算，不参与像素判定。

## 快照与预览

`Snapshot.Version` 是图像 ID/哈希、颜色/梯度/结构元素/迭代参数、种子点、量化规则和算法版本的 SHA-256。种子点先按坐标规范化排序，因此调用顺序不影响版本。

预览路径：

- 根据声明延迟选择目标最大维度 48（低预算进一步降到 32/16）；
- 实际倍率 `factor = ceil(maxDimension/targetDimension)`；
- 使用中心点采样降低分辨率；
- 生长迭代上限降到 12/8/4；
- 使用与精确路径完全相同的快照版本、量化规则和参数，只对种子坐标做整数缩放；
- 返回实际倍率、迭代数、耗时、是否在预算内和完整判定依据。

预览结束后的 `Finalize` 会检查最后一次预览快照与精确快照完全一致；混用返回 `E_VER`，错误携带 `left/right` 两侧版本。比较三项指标：区域数量差、最大重心偏差、最大面积相对误差；任一超限返回 `E_DIVERGE`，并携带三项实测值、首个不一致区域编号和首个差异像素坐标。

## 块、状态与检查点

每块包含 `grow -> clean -> stats` 三阶段，每阶段状态如下：

```text
pending -> processing -> committed
                    \-> failed -> processing
```

只允许上述迁移；其余迁移返回 `E_STATE` 且状态不变。任一块首次提交后冻结其快照版本。进度统一表示为“已提交阶段数/总阶段数”。提交顺序固定为块优先、阶段次优先的规范顺序，提交门保证数据库序列不交错且与并行完成顺序无关。

预算调度同时限制：

- 最大并发块任务数；
- 单块单阶段缓冲字节；
- 全局阶段缓冲字节。

预算不足时任务阻塞排队，并记录 `block_id`、阶段标签、需求量和当前使用量；不会静默丢块。

SQLite 每次阶段提交都在单个事务中写入阶段摘要、内容哈希、参数版本、算法版本、阶段产物，并更新已提交阶段计数。加载检查点会验证：

- 快照图像身份、真值指纹、参数/算法版本；
- 每条阶段产物 SHA-256；
- 规范阶段序号、阶段顺序、块几何；
- grow/clean 掩码完整且无重叠覆盖。

任一检查失败都会把整个快照标为 `invalid`，返回 `E_RESUME`；恢复执行随即全量重算，禁止部分旧阶段与新阶段混用。

## 三路径与验证器

验收比较三条完整路径：

1. 一次性全精度重算；
2. 先预览、再 `Finalize` 的精确重算；
3. 从 SQLite 检查点恢复。

三者必须生成相同 clean 掩码哈希、相同区域几何和相同规范阶段提交序列。

`cmd/verify` 不读取源图像，也不执行区域生长。它仅使用持久化阶段产物拼装 grow/clean 掩码、重算连通域统计，并使用 manifest 真值指纹与区域元数据做校验。若阶段产物被篡改，验证失败且恢复路径会自动废弃旧检查点。

## 实际验收结果

本次运行（预览耗时为当前机器单次实测，机器负载会造成小幅波动）：

```text
maskpreview demo: pass (seed=424242, samples=3)
icon_64 64x64 preview=2x budget=0.101/60ms regions=3 dev=0.707px area=10.50% pixel_equal=true stages_equal=true mask=f6697fee76309f0f
icon_96 96x96 preview=2x budget=0.165/60ms regions=3 dev=0.707px area=6.85% pixel_equal=true stages_equal=true mask=fcae677837ed8079
icon_144 144x144 preview=3x budget=0.334/60ms regions=3 dev=0.800px area=9.04% pixel_equal=true stages_equal=true mask=406d3ab1113f528a
verify[full]=pass samples=3
verify[incremental]=pass samples=3
error[parameter_range]=E_PARA ok=true
error[snapshot_mix]=E_VER ok=true
error[illegal_transition]=E_STATE ok=true
error[region_count]=E_REGION ok=true
error[preview_divergence]=E_DIVERGE ok=true
error[decode]=E_DECODE ok=true
error[metadata]=E_META ok=true
resource_budget completed=5 max_active=3 queued=2 ok=true
tamper_recovery reused=false recovered_same_mask=true
report=artifacts/run-report.json
```

关键结果：

| 样本 | 块/阶段数 | 精确真值重心 | 精确真值面积误差 | 预览倍率 | 预览重心误差 | 预览面积误差 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `icon_64` | 16 / 48 | 0 px | 0% | 2x | 0.707 px | 10.50% |
| `icon_96` | 36 / 108 | 0 px | 0% | 2x | 0.707 px | 6.85% |
| `icon_144` | 81 / 243 | 0 px | 0% | 3x | 0.800 px | 9.04% |

独立验证器三张样本均为 `truth_mask_hash_match=true`。三路径的 `pixel_consistent=true`、`region_stats_consistent=true`、`stage_sequence_consistent=true`。

自动化验证已执行：

```text
go test ./...       PASS
go test -race ./... PASS
go vet ./...        PASS
```

测试覆盖生成确定性、加载异常、参数区间、三路径一致性、快照混用、哈希/版本篡改、状态机非法迁移、预算排队、预览分歧结构化错误、不同并行度下输出稳定。

完整机器可读报告见 `artifacts/run-report.json`。

## 错误码

| 错误码 | 触发场景 |
| --- | --- |
| `E_DECODE` | PNG 损坏、图像哈希不符 |
| `E_META` | 分辨率/通道/真值声明不一致 |
| `E_PARA` | 参数或结构元素越界 |
| `E_REGION` | 区域数量与真值不一致 |
| `E_VER` | 预览/精确快照或版本混用 |
| `E_DIVERGE` | 预览与精确三项指标超差 |
| `E_STATE` | 非法阶段状态迁移 |
| `E_RESUME` | 检查点哈希/版本/完整性失效 |
| `E_RESOURCE` | 单任务超过硬缓冲上限 |

所有结构化错误都提供稳定错误码；涉及两侧值的异常同时提供字段名、`got/want` 或 `left/right`。
