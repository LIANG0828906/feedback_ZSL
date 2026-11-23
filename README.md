# Narrative Relay

面向叙事游戏的交互式叙事与分镜桌面工作台。项目从空仓库构建，包含确定性样本生成器、因果剧情引擎、版本化存档、编辑失效重演、SQLite 镜头检查点、受限工作线程池和完全离线的本地 Web 演示界面。

不使用 Docker、CGO SQLite 或外部账号。样本、真值、UI 资源和验证脚本均随代码库提供。

## 快速开始

要求 Go 1.22+：

```bash
go version
go env
go test ./...
go run ./cmd/workbench serve
```

打开 <http://127.0.0.1:8787>。

重新生成固定种子样本：

```bash
go run ./cmd/storygen -out samples -seed 20261124 -count 3
```

执行完整验收并写出诊断产物：

```bash
go run ./cmd/workbench demo \
  -samples samples \
  -artifacts artifacts/verification \
  -db data/verification.db \
  -saves data/verification-saves \
  -workers 4 \
  -buffer 8
```

验收命令会检查每份样本的每个镜头边界续演、编辑后重演、三路径比对，以及 SQLite 关闭后重开恢复。结果写入：

- `artifacts/verification/verification-report.json`
- `artifacts/verification/verification-summary.md`
- 每个工程的 `continuous.json`、`save-resume.json`、`edited-replay.json`、`invalidation.json`

本次实际验证结果见 [`VERIFICATION.md`](VERIFICATION.md)。

## SQLite 工具

Go 端使用纯 Go 驱动 `modernc.org/sqlite`，无需 CGO。CLI 仅用于人工检查数据库：

```bash
# Debian/Ubuntu
sudo apt install sqlite3

# Windows
winget install SQLite.SQLite

# macOS
brew install sqlite
```

当前环境没有免密 `sudo`，因此仓库内提供了解包后的 Linux CLI：

```bash
./bin/sqlite3 artifacts/verification/checkpoint-00.db \
  "SELECT * FROM checkpoint_meta;" \
  "SELECT COUNT(*), MIN(shot_seq), MAX(shot_seq) FROM shot_checkpoints;" \
  "PRAGMA integrity_check;"
```

## 工作台功能

- 因果图布局：汇聚边、数据读取边和互斥边分别显示。
- 节点检查器：前置标志位、角色状态条件、事件后果、镜头数与画幅优先级。
- 玩家选择时间轴：显示逻辑时间、目标节点和阻塞原因。
- 逐镜播放器：镜头呈现时刻、时长、画幅优先级、状态哈希与所属节点。
- 终态面板：最终标志位、角色状态和事件到达统计。
- 版本化存档：任意镜头边界另存、导出与续演。
- SQLite 检查点：逐镜事务写入与断点恢复。
- 编辑重演：直接编辑 `requires` / `events` JSON，自动计算失效闭包并原子替换。
- 三路径比对：连续推进、存档续演、编辑重演在受限线程池中并行执行。
- 失效热力图：标红失效后重演镜头，标绿复用旧结果的镜头。
- 诊断产物：比对失败时保留所有路径结果与首个差异定位。

## 确定性语义

1. 选择序列按数组顺序消费，`at_ms` 必须单调不减。
2. 选择目标节点只有在以下条件全部满足时才激活：
   - 所有 `join` 前置节点已完成；
   - 所有 `requires` 条件成立；
   - 不存在活跃的 `mutex` 冲突。
3. 节点激活时先按声明顺序应用 `events`，并记录事件到达顺序，再逐镜呈现。
4. 镜头起始时间 `max(播放时钟, 选择逻辑时间)`；镜头之间允许出现逻辑空档，但不得重叠。
5. 状态机只依赖工程内容、选择序列、参数和初始状态，不依赖线程、完成顺序或上一次呈现历史。
6. 同一工程与选择序列重复推进，逐镜呈现顺序、终态标志位和事件顺序完全一致。

## 错误码

| 错误码 | 触发条件 | 定位 |
|---|---|---|
| `E_SCHEMA` | 字段缺失、引用不存在、重复 ID、无效枚举 | `node_id` 与 `field` |
| `E_CYCLE` | `join` 因果图成环 | 环上节点与 `join` 字段 |
| `E_PARA` | 时长/时间戳非整数、越界、选择时间倒序 | 节点与数值字段 |
| `E_SAVE` | 存档结构版本、算法版本、内容哈希、指纹或进度冲突 | 冲突字段，禁止部分加载 |
| `E_DIVERGE` | 路径结果不一致 | 首个差异镜头序号、节点、字段 |

示例：

```text
E_PARA node=arrival field=shots[0].duration_ms: duration must be an integer in [1,86400000] ms
E_CYCLE node=arrival field=join: causal graph contains a cycle
E_SAVE node=archive field=content_hash: save content hash does not match the project
E_DIVERGE shot=7 node=gate field=state_hash: expected ..., got ...
```

## 存档与检查点

存档包含：

- `schema_version`、`algorithm_version`
- `project_id`、`content_hash`
- `fingerprint`（对语义状态做 SHA-256）
- 已消费选择偏移、逐镜进度、镜头历史
- 节点运行状态、标志位、角色状态、事件顺序、播放时钟
- 当前节点和当前节点下一镜索引

加载顺序为：完整解码 → 结构校验 → 项目/内容哈希校验 → 指纹校验 → 工程级镜头校验。任何冲突都返回 `E_SAVE`，不会构造部分会话。

SQLite 每个已验证镜头边界独立提交，元数据和镜头行在同一事务内写入。数据库使用单写连接与进程内互斥锁，重复提交幂等，跳号提交拒绝。恢复时逐行校验版本、哈希、保存载荷、镜头序号和连续性；任一行异常会整体删除该工程检查点。

## 编辑失效与复用

编辑某节点的前置条件或事件后果时：

1. 构建旧工程与编辑后工程的显式依赖图并取并集。
2. 从编辑节点出发，沿汇聚、数据读取和互斥关系计算失效闭包。
3. 对编辑后的工程执行完整确定性重演，作为一致性 oracle。
4. 未失效节点的新镜头必须与旧镜头语义字段逐项相等，才复制旧结果并标记 `reused=true`。
5. 失效节点镜头原子替换，标记 `invalidated=true`、`recomputed=true`。
6. 任何复用校验不一致立即返回 `E_DIVERGE`，旧运行结果不被修改。

工程内容哈希变化属于元数据变化。复用镜头保留原呈现时刻、时长、状态哈希等语义结果，同时更新当前工程内容哈希，并用 `original_content_hash` 显式记录旧值，不静默篡改。

## 配置与并发

配置文件示例见 [`workbench.example.json`](workbench.example.json)。

```json
{
  "address": "127.0.0.1:8787",
  "samples_dir": "samples",
  "checkpoint_db": "data/checkpoints.db",
  "saves_dir": "data/saves",
  "artifacts_dir": "artifacts/runs",
  "workers": 4,
  "buffer_per_worker": 8
}
```

```bash
go run ./cmd/workbench serve -config workbench.example.json
go run ./cmd/workbench serve -workers 2 -buffer 3 -addr 127.0.0.1:9000
```

- 固定工作线程数，最大并发由 `workers` 限制。
- 每个工作线程有独立有界缓冲；生产者阻塞等待，不丢任务。
- 结果按提交索引收集，不受完成顺序影响。
- 所有 SQLite 边界提交串行化并保持镜头顺序。

## Windows 本地路径

CLI 和服务均使用 Go `filepath`，可直接使用本地路径：

```powershell
go run .\cmd\workbench serve `
  -samples .\samples `
  -db .\data\checkpoints.db `
  -saves .\data\saves `
  -artifacts .\artifacts\runs
```

无需安装 C SQLite 开发库；`modernc.org/sqlite` 为纯 Go。SQLite CLI 只用于检查生成文件。

## 目录结构

```text
cmd/storygen/       固定种子样本生成器
cmd/workbench/      serve / validate / demo 命令
story/              工程模型、严格加载、校验、依赖图、内容哈希
engine/             确定性状态机、存档、比较、失效重演
checkpoint/         SQLite 串行检查点与恢复
worker/             有界固定线程池
samplegen/          多场景确定性样本与真值生成
workbench/          应用服务、HTTP API、保存索引、三路径比对
webui/              内嵌离线工作台界面
samples/            三份固定种子样本及 manifest 真值摘要
```

## 主要 API

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/api/projects` | 样本清单与检查点摘要 |
| `GET` | `/api/project?id=` | 工程、依赖图、基线运行 |
| `POST` | `/api/run` | 按给定选择序列推进 |
| `POST` | `/api/compare` | 连续/续演/编辑重演三路径并行比对 |
| `POST` | `/api/edit` | 编辑后重演预览 |
| `POST` | `/api/saves` | 创建版本化存档 |
| `POST` | `/api/resume` | 从存档续演 |
| `POST` | `/api/checkpoints/start` | 逐镜写入 SQLite |
| `POST` | `/api/checkpoints/resume` | 从最新检查点恢复 |
| `GET` | `/api/export` | 导出工程、运行、存档或检查点 |

`POST /api/compare` 的 `edits` 字段支持按顺序执行多个编辑；兼容单编辑 `edit` 字段。

## 测试

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build ./cmd/workbench ./cmd/storygen
```

测试覆盖字段缺失、浮点/负数时长、因果环、选择倒序、未知引用、逐边界续演、冲突字段、指纹损坏、编辑闭包、复用镜头、编辑序列、首个差异定位、SQLite 重启恢复、损坏整体作废、并发提交、线程池顺序与并发上限，以及 HTTP 工作流。

更详细的实现说明见 [`docs/architecture.md`](docs/architecture.md) 与 [`docs/schema.md`](docs/schema.md)。
