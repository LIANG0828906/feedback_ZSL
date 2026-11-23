# TEXEL FORGE

面向游戏贴图与互动媒体素材的**位深 / 通道布局转换与恢复诊断工作台**。

项目从零构建，只使用 Go 标准库，不使用 Docker、外部数据库或网络服务。Go 进程同时提供转换内核、版本化检查点与内嵌的本地可视化前端，浏览器访问 `127.0.0.1` 即可作为桌面工作台使用。

## 快速开始

要求 Go 1.22+。Windows 安装 Go 后，在 PowerShell 中执行：

```powershell
go version
go mod init textureworkbench   # 本仓库已完成，可跳过
go run ./cmd/workbench sample --seed 42 --out .\data
go run ./cmd/workbench preflight --data .\data --checkpoints .\data\checkpoints
go run ./cmd/workbench validate --descriptors .\data\descriptors
go run ./cmd/workbench verify --data .\data
go run ./cmd/workbench serve --data .\data --checkpoints .\data\checkpoints
```

打开 <http://127.0.0.1:8787>。首次启动若 `data/manifest.json` 不存在，`serve --auto-generate=true` 会按种子 42 自动生成样本。

Windows 也可直接执行 `.\scripts\build-windows.ps1`，脚本会运行测试、生成样本、预检并构建 `texel-forge.exe`。

## 交付内容

| 路径 | 说明 |
| --- | --- |
| `cmd/workbench` | `sample` / `preflight` / `validate` / `verify` / `serve` 命令入口 |
| `internal/tex` | 布局与目标描述校验、逐像素重排、位深量化、误差、直方图、格式编解码 |
| `internal/sample` | 固定种子的确定性样本与目标描述生成器 |
| `internal/engine` | 像素块状态机、事件统计、工作池、检查点、恢复、三路径比对 |
| `internal/server` | 本地 HTTP API 与内嵌工作台页面 |
| `scripts/build-windows.ps1` | Windows 测试、样本生成、预检与 EXE 构建脚本 |
| `data` | 已生成的 seed=42 样本、描述 JSON 与清单 |

### 确定性样本

生成器固定产出 10 个不同分辨率、不同位深和布局的样本：

- 8 位：`gray8`、`rgb8`、`rgba8`、`bgr8`、`bgra8`
- 16 位：`gray16`、`rgb16`、`rgba16`、`bgr16`、`bgra16`
- 图像包含已知色块、分通道渐变、棋盘区与 16 位低比特纹理。
- 同时生成 12 个目标描述组合，覆盖 8/16 位、灰度/RGB/RGBA/BGR/BGRA、通道增加、通道减少、补零与重复 alpha。

相同种子和输出目录重复运行时，所有样本文件与 `manifest.json` 字节级一致。

## 目标描述

示例：

```json
{
  "id": "rgba8-to-bgra16-repeat",
  "source_image": "rgba8",
  "source_layout": "rgba",
  "target_layout": "bgra",
  "target_bit_depth": 16,
  "permutation": [2, 1, 0, 3],
  "fill": "repeat_alpha"
}
```

`permutation` 是**源通道索引的完整排列**，长度为源布局通道数。转换时目标第 `i` 个槽位取 `source[permutation[i]]`，直到目标通道被填满：

- 通道数增加：剩余目标槽位按 `fill` 填充。
  - `zero`：填 0。
  - `repeat_alpha`：重复“最接近 alpha 的源通道”。若源有 alpha，则用源 alpha；否则按目标 alpha 前一通道的语义在源中寻找，例如 RGB→RGBA 使用 B，RGB→BGRA 使用 R，灰度→RGBA 使用 Y。
- 通道数减少：未进入目标槽位的源通道登记为丢弃通道。
- 逆映射查询：`source_from_target[source_channel] = target_slot`。若存在丢弃通道，返回 `E_LOSS` 和完整的丢弃通道标识。

### 描述校验错误

| 错误 | 触发条件 |
| --- | --- |
| `E_PARA` | 字段缺失、位深不是 8/16、布局非法、填充枚举非法、重复 alpha 与目标通道数/alpha 不相容 |
| `E_LAYOUT` | 置换长度不符、重复通道索引、索引越界；错误体包含 `channel_index` |
| `E_LOSS` | 查询不可逆映射或发生通道丢弃；错误体包含 `dropped_channels` |

## 转换与量化

源样本以自定义 `TWBIMG01` 确定性二进制格式保存，内部始终持有 `uint16` 样本，8 位源值范围为 `0..255`，16 位源值范围为 `0..65535`。

量化使用整型四舍五入：

```text
out = round(value * targetMax / sourceMax)
back = round(out * sourceMax / targetMax)
error = back - value
```

因此：

- 8→16 是精确扩展（`value * 257`）；
- 16→8 保留每样本量化残差；
- 8→8、16→16 量化误差为 0；
- 统计包括总平均绝对误差、最大绝对误差、RMSE、非零误差数与逐通道统计。

输出尺寸严格等于源尺寸，目标通道数由目标布局决定。导出 PNG 保留目标位深；BGR/BGRA 仅在 PNG 表达层转换到标准 RGB/RGBA，不改变内部目标通道布局。`twimg` 导出保留原始布局顺序。

## 像素块状态机

每张图按固定 `block_width × block_height` 切块，状态仅允许：

```text
pending -> processing -> committed
                       -> failed
failed  -> processing
```

非法迁移返回 `E_STATE`，原状态不变。已提交块保存输出像素、逐像素量化误差、输出哈希、目标描述版本、参数版本和算法版本。

工作池并行转换块，但提交由单一收集器按块序号推进：

- `max_workers` 限制工作线程数；
- `buffer_pixels_per_worker` 必须不小于单块像素数；
- 有界任务队列满时记录 `buffer_queue_waits` 与原因，并阻塞排队，不丢块；
- 单块内部异常只将该块置为 `failed`，其他块和已提交结果不受影响；
- 点击失败块或调用重试 API 后，`failed -> processing`，成功则提交。

### 描述变化与已提交结果

已提交块只读，不会被配置操作原地改写。目标位深、目标布局、置换、填充策略或参数版本发生变化时，系统显式执行**整代快照作废**：

1. 保留旧代失效块的索引、矩形、状态、旧描述版本和输出哈希；
2. 清理旧检查点，全部块生成新的 `pending` 快照；
3. 在 API、矩阵和诊断流中标注失效块与原因；
4. 重新计算，不把不同目标描述的块混用。

这同时满足“已提交结果不被静默改写”和“目标描述变化时快照整体识别并作废”两个约束。

## 版本化持久化检查点

检查点目录结构：

```text
data/checkpoints/<session-key>/
  checkpoint.json
  blocks/00000000.json
  blocks/00000001.json
  invalidations/invalidated-<timestamp>.json
```

每个块记录包含：

- 块序号 / 行列 / 矩形；
- 源内容哈希；
- 目标描述版本、参数版本、算法版本；
- 输出摘要与逐像素误差摘要；
- 输出内容哈希、误差内容哈希；
- 原始输出与误差负载。

提交协议在存储互斥锁内串行执行：

1. 同目录临时文件写入块记录并 `fsync`；
2. 原子 `rename` 为正式块文件；
3. 更新 `checkpoint.json`，同样使用临时文件 + `fsync` + 原子 `rename`；
4. 全部成功后才把内存块置为 `committed`。

崩溃窗口中的未入清单块会被忽略；不会出现部分写入但被当作已提交的情况。加载时检查清单头及逐块版本与哈希，只要内容哈希、目标描述版本、参数版本、算法版本任一不匹配，就**整体作废**快照并从源图像重算，绝不混用旧块。

取消或强制终止后，重启只恢复已持久化且版本匹配的块。导出接口只允许完整提交的快照；不会承诺崩溃后仍能导出完整结果。

## 三路径一致性

自动比对同时运行：

1. **实时分段**：工作池逐块转换、逐块提交；
2. **断点恢复**：先提交一半块，关闭会话，重新打开并从检查点恢复，再完成剩余块；
3. **一次性全量重算**：不经过块状态机，直接调用同一像素内核全图转换。

三条路径的比较顺序为像素、逐像素量化误差、量化统计、通道直方图和映射元数据。若不一致，返回 `E_DIFF`，并定位：

- 转换阶段；
- 首个不一致像素坐标 `x/y`；
- 通道号；
- 所属块序号；
- 两侧取值。

## 工作台界面

页面包含：

- 源图 / 转换结果并排预览；
- 逐通道 256 桶直方图与波形叠加；
- 逐像素量化误差热力图（取每个像素各通道最大绝对误差）；
- 像素块状态矩阵、缓存命中、失效斜线标记与说明；
- 三路径重算次数、缓存命中、耗时；
- 逆映射、填充、丢弃通道与 alpha 最近通道关系；
- 路径诊断、状态演进、版本失配与异常日志；
- 目标描述、参数版本、块尺寸、最大并行度、每线程像素缓冲、队列深度；
- 开始/继续、暂停、取消、逐块重试、故障注入、三路径比对、PNG/TEX 导出。

修改目标描述时界面要求明确确认“整体作废当前快照”，并显示失效块数量，不会静默覆盖已提交结果。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/bootstrap` | 样本、描述、算法、预检信息 |
| POST | `/api/sessions` | 创建/恢复会话 |
| GET | `/api/sessions/{id}` | 状态、块矩阵、直方图/统计 |
| POST | `/api/sessions/{id}/run` | 启动/继续分段处理 |
| POST | `/api/sessions/{id}/pause` | 停止调度新块 |
| POST | `/api/sessions/{id}/cancel` | 取消未调度块 |
| POST | `/api/sessions/{id}/reconfigure` | 显式整代作废并换描述 |
| POST | `/api/sessions/{id}/compare` | 三路径逐像素比对 |
| POST | `/api/sessions/{id}/blocks/{index}/retry` | 逐块重试 |
| POST | `/api/sessions/{id}/blocks/{index}/fault` | 注入单块异常 |
| GET | `/api/sessions/{id}/mapping/inverse` | 逆映射或 `E_LOSS` |
| GET | `/api/sessions/{id}/preview?side=source\|target` | 预览 PNG |
| GET | `/api/sessions/{id}/heatmap` | 量化误差热力图 |
| GET | `/api/sessions/{id}/export?format=png\|twimg` | 完整快照导出 |
| GET | `/api/samples/{id}/preview` | 源样本预览 |

错误统一为：

```json
{
  "code": "E_LAYOUT",
  "field": "permutation",
  "channel_index": 2,
  "message": "duplicate channel index in permutation"
}
```

## 验证

执行：

```bash
go test ./...
go test -race ./internal/...
go vet ./...
go run ./cmd/workbench verify --data ./data
```

实测环境：Go `go1.22.12`，`linux/amd64`（代码不使用平台专属 API，可在 Windows 运行）。

内置验收命令实际输出：

```text
== three-path consistency ==
  MATCH  rgb8-to-bgra8-zero       rgb -> bgra/8 bit  paths=3
         realtime_segmented   recompute=12   cache=0    time=2.127ms
         checkpoint_resume    recompute=12   cache=6    time=4.360ms
         full_recompute       recompute=12   cache=0    time=0.271ms
  MATCH  rgba8-to-rgb8-drop       rgba -> rgb/8 bit  paths=3
         realtime_segmented   recompute=12   cache=0    time=1.733ms
         checkpoint_resume    recompute=12   cache=6    time=2.847ms
         full_recompute       recompute=12   cache=0    time=0.250ms
  MATCH  rgb16-to-rgba16-repeat   rgb -> rgba/16 bit  paths=3
         realtime_segmented   recompute=12   cache=0    time=2.029ms
         checkpoint_resume    recompute=12   cache=6    time=3.128ms
         full_recompute       recompute=12   cache=0    time=0.252ms
  MATCH  bgra16-to-rgb8-drop      bgra -> rgb/8 bit  paths=3
         realtime_segmented   recompute=12   cache=0    time=1.686ms
         checkpoint_resume    recompute=12   cache=6    time=2.669ms
         full_recompute       recompute=12   cache=0    time=0.272ms
== version mismatch invalidates whole snapshot ==
  INVALIDATED reasons=[target_descriptor_version_mismatch] restored=0 committed=0
VERIFY OK
```

额外实测：

- 工作台服务创建 `rgba8 → BGRA/16bit` 会话，12 个块全部提交，PNG 预览与 16 位 PNG 导出成功；
- `bgra16 → RGB/8bit` 路径 `mean_abs=18.6743`、`max_abs=128`，证明降位深误差可测；
- 停止 Go 服务后重新启动，用相同图像/描述/参数重新创建会话，恢复 3 个已提交块并继续剩余 9 块；
- 恢复路径最终三路径比对 `match=true`；
- 通道减少的逆映射返回 `E_LOSS` 并携带 `A(src[3])`。

测试覆盖还包括：描述字段错误与冲突通道、8/16 位量化方向、BGR/BGRA 重排、补零与重复 alpha、逆映射成功/损失、非法状态迁移、版本失配整体作废、参数版本失配、断点恢复、失败块重试隔离、检查点提交顺序、缓冲上限与排队、暂停恢复、三路径 8/16 位及通道增减、首个差异定位、损坏文件 `E_DECODE`。

## 边界与约束

- 工作台是本地单机进程 + 浏览器 UI，不监听公网，不访问外部服务。
- 当前用户必须对样本目录和检查点目录有读写权限；`preflight` 会实际创建并删除测试文件。
- 强制终止只能保证已调用提交协议的块可恢复；正在处理或尚未提交的块不会出现在恢复结果中。
- 目标描述变化会整体作废旧快照，这是为保证所有输出像素属于同一目标描述而采用的有意设计。

