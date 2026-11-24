# Resource Distribution Workbench

面向开放世界资源采集点分布设计验证的本地工作台。项目包含确定性样本生成、区域结构校验、分层采样估计、采集点重建、密度热力面、地带级失效传播、增量重算、偏差上界报告、SQLite 分地带检查点，以及 Wails 诊断界面。

核心实现不依赖网络或外部服务。隐藏真值只用于验收，估算、重建、热力面和偏差上界计算均不读取真值。

## Quick Start

环境要求：

- Go 1.22+
- Wails CLI 2.10.2（桌面界面）
- Windows 需要 WebView2 Runtime
- SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，无需安装 `sqlite3`
- 前端是内嵌静态 HTML/CSS/JS，不需要 Node/npm

建议先配置本地 Go 环境：

```bash
go env -w GOPATH="$HOME/go"
go env -w GOPROXY="https://proxy.golang.org,direct"
go install github.com/wailsapp/wails/v2/cmd/wails@latest
```

Windows 请先安装 WebView2 Runtime，再运行 `wails doctor`。

```bash
go mod download
go test ./...
go run ./cmd/workbench sample -seed 42 -out samples/verdant-reach.json
go run ./cmd/workbench validate -region samples/verdant-reach.json
go run ./cmd/workbench run -region samples/verdant-reach.json -db workbench.sqlite -out report.json
go run ./cmd/workbench truth-check -region samples/verdant-reach.json -db workbench.sqlite
go run ./cmd/workbench compare -region samples/verdant-reach.json -db workbench.sqlite
```

桌面界面：

```bash
wails doctor
wails dev -tags wails
# production build
wails build -tags wails
```

界面启动后默认加载 `samples/verdant-reach.json` 的嵌入副本，并把检查点写入用户配置目录下的 `resource-workbench/checkpoints.sqlite`。也可以输入其他本地路径，或把 DB 路径设为 `none` 关闭持久化。

## CLI

| 命令 | 用途 |
| --- | --- |
| `sample` | 按固定种子生成本地确定性区域、采样观测与隐藏真值 |
| `validate` | 校验 schema、几何、栅格、采样点和真值清单 |
| `run` | 加载或恢复工作区，计算缺失/失效地带并提交检查点 |
| `report` | 与 `run` 相同，并导出完整 JSON 报告 |
| `truth-check` | 用隐藏真值核对密度、稀有度、点数和热力面容差 |
| `compare` | 将当前增量缓存与空缓存全量重建逐项比较 |
| `edit-zone` | 修改密度上限或稀有度覆盖值，触发精确增量重算 |
| `edit-nocollect` | 替换/移动/删除禁采区边界 |
| `retry-zone` | 显式重试一个地带及其下游，生成新的检查点代次 |

示例：

```bash
go run ./cmd/workbench edit-zone \
  -region samples/verdant-reach.json -db workbench.sqlite \
  -zone zone-1-west -density-limit 0.09

go run ./cmd/workbench edit-nocollect \
  -region samples/verdant-reach.json -db workbench.sqlite \
  -id nc-grove -polygon "45,35;60,35;60,50;45,50;45,35"

go run ./cmd/workbench retry-zone \
  -region samples/verdant-reach.json -db workbench.sqlite \
  -zone zone-2-northeast
```

## Architecture

```text
cmd/workbench/                 CLI 与本地验收入口
internal/workbench/            校验、几何、采样、估计、重建、偏差、调度、失效
internal/store/                SQLite 检查点、快照元数据、追加式地带提交历史
frontend/dist/                 无构建步骤的 Wails 静态诊断界面
build/                         Wails 应用图标、Windows manifest/installer 资源
app_wails.go                   Wails 绑定 API
main_wails.go                  Wails 应用入口（build tag: wails）
main_headless.go               默认无 GUI 入口，提示 CLI/桌面命令
samples/verdant-reach.json     固定种子 42 的本地确定性样本与隐藏真值
docs/SCHEMA.md                 数据模型、错误码、算法不变量
docs/VERIFICATION.md           实际验证命令与结果
```

### Deterministic model

- 固定内容哈希基于不可变区域内容计算。真值、可编辑地带参数和禁采区不进入该哈希。
- 每个地带参数版本包含自身密度上限/稀有度覆盖、相交禁采区、上游地带版本和算法版本。
- 采样格点按 `rankKey(baseHash, zone, terrain, siteID)` 排序，不依赖随机数状态或 map 遍历。
- 重建坐标由 `SHA-256(baseHash, paramVersion, zoneID, attempt)` 派生；线程 ID、worker 编号和完成顺序不参与抽样。
- 并行 worker 只负责计算，提交始终按确定拓扑序串行进行。最终提交序列等于拓扑序。

### Sampling and estimation

- 地带有效面积按地形栅格中心点确定，排除地带外、非允许地形和禁采区，使用固定栅格面积。
- 每层为 `zone × terrain_type`，按有效面积分配样本，至少一条样本（满足上限时）。
- 地带密度是各层样本均值的面积加权组合；标准误由层内样本方差和权重传播。
- 稀有度按观测计数归一化，再按 `rarity_step` 最大余数取整。
- 密度上限是硬约束；低于观测均值时裁剪，并记录 cap slack。

### Reconstruction and heat surface

- 地带目标点数 `round(estimated_density × effective_area)`。
- 采样使用拒绝法：拒绝区域外、地带外、禁采区、非法地形和非法稀有度层。
- 采集点按 `(zone_id, x, y, id)` 固定排序，点摘要与热力面摘要均使用 SHA-256。
- 热力面使用固定 `heatmap_cell_size` 栅格；单元格中心落在有效地带的点写入地带密度，禁采区写 0。

### Invalidation and incremental execution

显式依赖图包含：

```text
density_limit ─┐
rarity_mix ────┼─> zone.params ─> sampling ─> estimate ─> reconstruction ─> heat
no_collect ────┘                                  │
upstream zone ────────────────────────────────────┴─> downstream
all zone heat ─> global.surface ─> global.bias
```

编辑地带参数或禁采区时：

1. 只标记直接受影响地带及依赖图中的下游地带。
2. 未受影响地带直接复用内存/持久化结果。
3. 仅对失效地带并行重算，再按拓扑序串行提交。
4. 全局热力面由各地带缓存层确定性合并，偏差报告由缓存指标重算。

`compare` 会以相同状态从空缓存全量重算，逐地带核对样本索引、密度、标准误、稀有度、点数、点集合、热力层、指标，并核对全局热力面、提交顺序和偏差报告。首个不一致返回 `E_DIVERGE`，带 `zone_id` 与 `data_item`。

### Bias report

每个地带输出四项观测指标和累积上界：

- `density_quantization_error`：样本密度到声明步长的量化误差
- `standard_error`：层内方差传播后的地带标准误
- `rarity_rounding_residual`：稀有度配比取整残差
- `heatmap_reconstruction_error`：热力面在样本格点处的重建误差

累积上界仅使用样本点数、有效面积、观测密度、层内方差、密度/稀有度步长和实际密度裁剪量。任一累积上界超过 `bias_threshold` 时返回 `E_BIAS`，包含首个超限地带、指标和上界值。隐藏真值不参与上界推导。

### SQLite checkpoint and recovery

表结构：

- `snapshot_meta`：快照 ID、区域 ID、不可变内容哈希、状态 JSON/哈希、算法版本。
- `zone_checkpoints`：地带 ID、拓扑序、代次、全局提交序、参数版本、样本索引、估计、点摘要、热力面摘要、完整结果和历史 `is_current` 标记。

每提交一个地带使用单连接 + 事务完成：退休旧 current 行、插入新代次、更新状态。数据库最大连接数为 1，引擎提交又按拓扑序串行，因此写入不会交错。旧代次保留为追加历史，不被静默覆盖。

启动恢复规则：

- 快照基础内容哈希/算法版本/状态哈希任一不匹配：删除整个快照并全量重建。
- 任一 current 地带的参数版本、算法版本、点摘要或热力面摘要不匹配：整体作废，禁止部分复用。
- 缺少某个地带检查点但其余都有效：只重算缺失地带，这是断点续算场景。

## Error Contract

| Code | 触发场景 | 定位字段 |
| --- | --- | --- |
| `E_SCHEMA` | 缺失字段、非法上限、未知依赖、采样数超声明上限 | `field` |
| `E_GEOM` | 多边形未闭合/自交/退化、地带重叠、栅格外形不一致 | `object_id` |
| `E_SAMPLE` | 样本/编辑后样本落入禁采区、越界、地形冲突 | `point` + `object_id` |
| `E_BIAS` | 某地带累积偏差上界超阈值 | `zone_id` + `value` + `data_item` |
| `E_DIVERGE` | 增量结果与全量重建首个数据项不一致 | `zone_id` + `data_item` |
| `E_STORE` | SQLite 打开/事务/恢复失败 | `message` |
| `E_IO` | 文件读写、取消或内部输入失败 | `message` |

## Concurrency

- `MaxParallel` 受 `sampling_config.max_parallel` 约束。
- 每个 worker 的样本暂存上限受 `per_thread_sample_buffer` 约束。
- 样本数超过单线程缓冲时按批推进并记录 `sample_buffer_exhausted` 事件；事件包含地带、worker、阻塞和计数。
- 并行与串行使用同一确定性样本选择/坐标派生函数；测试和 `CompareFull` 验证最终点集、热力面与拓扑提交序一致。

## Wails Notes

当前开发容器已安装 Wails CLI v2.10.2，并成功执行 `go build -tags wails .`。`wails doctor` 显示该 Linux 容器缺少 `pkg-config`/`libwebkit2gtk`，所以无法在此容器完成原生窗口打包或启动；前端使用内嵌静态资源，不依赖 npm。

Windows 上确认 WebView2 已安装后可直接：

```powershell
wails doctor
wails dev -tags wails
wails build -tags wails
```

若 Linux 需要原生运行：

```bash
sudo apt install pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
wails build -tags wails
```

完整实测记录见 `docs/VERIFICATION.md`。
