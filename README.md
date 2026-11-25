# 多屏互动与投屏协议内核

本仓库实现一个本地、确定性的投屏协议内核，覆盖会话状态机、拓扑/能力校验、整数坐标映射、同刻事件裁决、SQLite 帧窗口检查点、断线恢复、三路径一致性比对与异常注入。

实现不依赖容器、外部服务或 GPU。SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`，Go 版本要求 1.22+。

## 快速开始

```bash
go mod tidy
go run ./cmd/castgen -seed 42 -screens 4 -windows 12 -sessions 2 -out samples/sample.json
go run ./cmd/castctl verify -sample samples/sample.json
go test ./...
```

`castctl verify` 会同时执行：

- 生成器真值比对；
- 实时推进、一次性全量重放，以及从每个窗口断点恢复后的完整重放；
- SQLite 检查点行顺序与恢复结果比对；
- `E_DECODE/E_ORDER/E_CAP/E_SEG` 四类异常链路检查。

本机能力检查：

```bash
go run ./cmd/castctl doctor
```

该命令验证当前目录可创建 SQLite 文件，并验证 `127.0.0.1` 本地回环可用。Windows 上使用同一 Go/SQLite 实现即可；内核本身不启动网络服务。

## 目录

| 路径 | 作用 |
| --- | --- |
| `protocol/` | 数据模型、JSON 加载、结构/引用校验、时间调度、错误模型 |
| `kernel/` | 状态机、并行映射、帧提交、仲裁、检查点快照、三路径比较 |
| `store/` | SQLite 检查点，串行事务写入与整体失效校验 |
| `samplegen/` | 固定种子样本与独立解析真值生成器 |
| `verify/` | 真值比对、恢复验证、异常注入链路 |
| `cmd/castgen/` | 样本生成命令 |
| `cmd/castctl/` | 验证、运行、异常样本生成、环境检查命令 |
| `samples/sample.json` | 已生成的固定种子样本 |

详细设计见 `docs/DESIGN.md`，实测记录见 `docs/VALIDATION.md`。

## 输入模型

顶层样本清单包含多个会话与对应真值：

```json
{
  "seed": 42,
  "screens": 4,
  "windows": 12,
  "sessions": [
    {
      "schema_version": 1,
      "session_id": "session-000042-00",
      "algorithm_version": "castmap-v1",
      "master": { "id": "master", "topology": {}, "capability": {} },
      "displays": [],
      "initial_displays": ["screen-a"],
      "events": [],
      "windows": [],
      "total_windows": 12
    }
  ],
  "truths": []
}
```

关键约定：

- 缩放比使用有理数 `"num/den"`，同时接受 JSON 数字；映射全程使用整数乘除，不经过浮点。
- `displays[].topology` 与 `displays[].capability` 的分辨率、缩放比必须一致。
- 逻辑宽高必须满足 `width * scale_num == resolution.width * scale_den`，高度同理。
- 帧窗口 `region` 使用主控逻辑世界坐标；`targets` 为空表示当前全部已加入受控屏。
- 受控屏加入/退出、拓扑变更、状态控制和帧窗口按逻辑时间归并；同刻默认相位为拓扑、退出、加入、状态控制、帧。

## 状态机

| 当前状态 | 合法指令 | 下一状态 |
| --- | --- | --- |
| `established` | `negotiate` | `negotiating` |
| `negotiating` | `start`（至少一个已加入屏） | `casting` |
| `casting` | `frame` | `casting` |
| `casting` | `pause` | `paused` |
| `paused` | `resume` | `casting` |
| 任意非终态 | `join` / `leave` / `topology` | 状态不变 |
| 任意非终态 | `end` | `ended` |

非法指令返回 `E_STATE`，游标、状态与已提交进度均不变化，调用方可原位重试。`ended` 后任何指令都被拒绝。

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_DECODE` | 输入 JSON 损坏或反序列化失败 |
| `E_SCHEMA` | 必填字段缺失、结构非法、标识重复 |
| `E_REF` | 引用未知端，或帧目标尚未加入 |
| `E_ORDER` | 事件序号或逻辑时间倒置 |
| `E_CAP` | 能力声明与拓扑冲突 |
| `E_PARA` | 分辨率/缩放比越界，错误携带屏幕与字段 |
| `E_STATE` | 状态迁移非法 |
| `E_CONFLICT` | 同刻端事件无法裁决、重复加入或退出未加入端 |
| `E_SEG` | 单窗口内部注入失败，可重试且不污染其他窗口 |
| `E_DIFF` | 三路径或真值比对出现首个差异 |
| `E_STORE` | SQLite 检查点写入/读取失败 |

错误对象包含 `source`、`path`，并在适用时包含 `time`、`window`、`display_id`、`field`。

## 映射与并发

每个受控屏映射：

1. 求主控逻辑区域与受控屏逻辑拓扑的交集；
2. 将交集平移到屏内世界坐标；
3. 按有理缩放比和声明的 `floor/ceil/nearest/outward/inward` 取舍方向映射到像素；
4. 裁剪到分辨率范围；
5. 同时输出 `pixel`、`world`（主控世界坐标）和 `screen_world`（屏内世界坐标）。

窗口内的屏映射由固定分片工作池并行执行。`max_workers` 限制并行度，`per_worker_pixel_bytes` 限制每个线程的像素暂存缓冲；大区域按块复用缓冲，缓冲/工作线程占用时排队，并在 `queue_records` 中记录原因，不会静默丢弃屏幕。

## 检查点与恢复

每个帧窗口提交时，内核在单连接、互斥、`BEGIN/COMMIT` 事务中写入：

- 该窗口各受控屏区域与映射参数；
- 当前端集合和完整声明拓扑；
- 拓扑版本；
- 完整会话内容哈希；
- 算法版本；
- 快照哈希及可恢复运行状态。

加载时逐行校验窗口连续性、快照哈希、内容哈希、算法版本、拓扑版本和 `NextAction`。任一行任一版本不匹配，都会删除该会话全部快照并返回 `Invalidated`，随后必须全量重放，禁止混用旧状态。

强制终止后可恢复的边界仅为“已持久化且版本匹配的帧窗口”。本实现不声称强制终止后仍可生成退出报告；`verify` 报告中的 `forced_stop_exit_report` 固定为 `false`。

## 仲裁规则

同一逻辑时刻的加入/退出事件按以下规则排序并写入裁决明细：

1. 退出先于加入；
2. 同相位按受控屏标识升序，再按事件序号升序；
3. 同一屏在同一时刻出现多个加入/退出操作视为不可裁决，返回 `E_CONFLICT`；
4. 重复加入、退出未加入屏同样返回 `E_CONFLICT`。

处于 `casting` 或 `paused` 时新屏加入，会立即使用最新已提交窗口区域按新屏拓扑重映射并回放，而不是等待下一个窗口。

## 三路径一致性

`verify.VerifySample` 对每个会话执行：

- 一次性全量重放；
- 实时逐指令推进并逐窗口持久化；
- 在 1..N 每个窗口断点关闭/重开 SQLite 后恢复并继续。

比较范围包括逐窗口屏幕区域、映射参数、端集合、拓扑版本、队列记录、裁决明细、事件生效记录、加入回放和会话终态。首个差异返回 `E_DIFF`，并携带窗口号、屏幕标识和字段名。
