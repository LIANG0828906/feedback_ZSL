# AudioForge：多人对战音效包络与分包诊断

这是一个可直接在本机运行的 Go 1.22+ 工程，用于验证以下完整链路：

```text
三发行分支事件流
  -> 字段语义重映射 / tick 与毫秒整数换算
  -> 条件事件过滤
  -> 通道内优先级、禁止打断、到达顺序合并
  -> 通道包络模板叠加与定点量化
  -> 确定性切包与线格式编码
  -> SQLite 窗口检查点
  -> 回环模拟声卡回显对齐
  -> Web 诊断界面
```

工程不依赖 Docker、外部服务、网络账号或音视频文件。启动时按固定种子生成三份等价但字段语义不同的分支样本、能力清单与真值清单。**验收只使用生成器构造的真值。**

## 快速运行

环境要求：Go 1.22 或更高版本。本项目使用 `modernc.org/sqlite` 纯 Go 驱动，建议关闭 cgo，避免 Windows/精简环境缺少 C 编译器：

Windows CMD：

```bat
set CGO_ENABLED=0
go mod download
go test -timeout 90s ./...
go run ./cmd/audioforge
```

Windows PowerShell：

```powershell
$env:CGO_ENABLED=0
go mod download
go test -timeout 90s ./...
go run ./cmd/audioforge
```

Linux/macOS：

```bash
CGO_ENABLED=0 go test -timeout 90s ./...
CGO_ENABLED=0 go run ./cmd/audioforge
```

默认从 `config.json` 读取配置：

- 诊断界面：`http://127.0.0.1:18080`
- 模拟设备服务：`http://127.0.0.1:18081`
- 生成样本：`data/samples/`
- SQLite 检查点：`data/checkpoints.db`

只跑一次完整诊断并输出 JSON：

```bash
CGO_ENABLED=0 go run ./cmd/audioforge -diagnose
```

所有监听都显式绑定 `127.0.0.1`，不需要管理员权限。端口来自配置，改为 `0` 时由操作系统分配临时端口。

## 分支语义

三个分支使用同一个 JSON 容器，差异由分支声明决定，而不是容器层级：

| 分支 | 时间基准 | `events[].value` 语义 | `events[].sequence` 语义 |
|---|---|---|---|
| `arena-tick` | tick | `intensity` | `event_types` |
| `mobile-ms` | 毫秒 | `duration_samples` | `channel_routes` |
| `console-tick` | tick | `duration_samples` | `channel_routes` |

跨分支必要的伴随字段用于同时保留强度与持续采样数：

- `intensity` 语义分支把强度放在 `value`，持续采样数放在 `duration_samples`。
- `duration_samples` 语义分支把持续采样数放在 `value`，强度放在 `intensity`。
- `event_types` 语义分支的类型来自 `sequence`，通道路由来自 `route`。
- `channel_routes` 语义分支的路由来自 `sequence`，事件类型来自 `kind`。

加载后每个内部事件保留 `source_branch`、`source_ordinal` 和 `source_paths`。例如：

```text
start      -> events[0].time
intensity  -> events[0].value
routes     -> events[0].sequence
```

未知分支或必填字段缺失返回 `E_VERSION`，错误中带分支名和字段路径。tick/毫秒换算与强度/持续数换算全程使用 `int64`，不做浮点累积；溢出返回 `E_OVER`，带事件编号和字段名。

## 确定性合成规则

每个逻辑通道独立合成，线程数只影响执行并行度，不影响结果：

1. 按 `start`、优先级降序、源到达序号、事件 ID 排序，得到稳定顺序。
2. 当前事件标记为禁止打断时，后续重叠事件被丢弃。
3. 否则更高优先级事件可截断当前事件；同级或更低优先级重叠事件被丢弃。
4. 距离当前事件起点小于 `min_event_interval` 且优先级不更高的事件被丢弃。
5. 条件事件只有在 `conditional=false` 或 `gate=true` 时进入后续合成。

通过筛选的事件按每通道 `template` 在时间轴上渲染，模板位置使用纯整数线性映射：

```text
templateIndex = relativeSample * (templateLen - 1) / (durationSamples - 1)
raw           = template[templateIndex] * intensity / 1000
raw           = raw * gainPermille / 1000
quantized     = round(raw / quantUnit)   // 支持 floor / ceil / nearest
```

最终输出定点整型通道包络帧。生成器真值由独立实现构造，并在若干采样点、包边界和条件事件激活状态上校验。

## 切包与依赖失效

每个通道的包序号从 `0` 独立递增，包包含通道、包序号、起始采样偏移、采样数、时序标记、前包占位偏移、定点包络采样、依赖事件集合、已消费事件偏移和内容哈希。

任一包超过 `max_packet_samples` 或时序标记不在 `timing_window` 内返回 `E_PACK`，带通道、包序号、越界字段和采样偏移。相同帧与参数重切分使用稳定 JSON 编码，逐字节一致。

依赖图显式记录每个包的依赖事件集合、通道模板哈希、量化单位、前包占位偏移和内容哈希。重算时按通道找到第一个实际变化包，只失效该包及同通道后续包。受影响通道之前的前缀包以及完全未受影响的其他通道继续复用缓存。

诊断报告展示四类变化：

- 事件优先级编辑
- 量化单位调整
- 每包采样点数上限调整
- 新增事件

## 状态机与 SQLite

每个包的合法状态迁移：

```text
pending_quantization -> quantizing -> sliced -> committed
          \                \            \
           +----------------+------------+-> failed
```

`committed` 与 `failed` 为终态。非法迁移返回 `E_STATE`，锁内校验并保持原状态。

SQLite 表：

- `snapshots`：作业、参数版本、算法版本、能力版本、完成状态
- `windows`：提交窗口序号和提交时间
- `packet_records`：包采样、边界、消费偏移、事件依赖、内容哈希与版本

每个窗口在单个事务中写入；写入口由互斥锁串行化，并校验通道内包序号、采样起点连续性、窗口顺序和哈希。加载时任一包哈希、参数版本、算法版本或窗口连续性不匹配，会删除整个快照，返回 `E_SNAPSHOT`。

## 三条执行路径

1. **实时合成**：正常合成、切包、逐窗口提交。
2. **断点恢复**：强制停止后只从已提交窗口恢复；已提交前缀必须与确定性重算逐字节一致，未提交窗口继续合成并提交。
3. **持久化重放**：只读取 SQLite 中的包决策，直接重建包络帧、切分边界与提交窗口，不再依赖内存事件。

主动取消返回 `E_CANCEL`，不会留下可恢复快照；强制终止返回 `E_TERMINATED`，保留已提交窗口用于恢复。三路径比对会定位首个不一致采样偏移、通道和包序号并返回 `E_DIFF`。

## 回环设备与异常

模拟设备端点：

```text
GET  /healthz
POST /v1/packets   解析并校验一个音频包批次，回显实际施加的采样点
POST /v1/faults    注入 drop / duplicate / delay_ms
GET  /v1/echo      读取回显
POST /v1/reset     清空状态
```

`POST /v1/packets` 的声明格式：

```json
{
  "packets": [
    {
      "seq": 0,
      "channel": 0,
      "start_sample": 0,
      "sample_count": 96,
      "timing_mark": 0,
      "previous_offset": 0,
      "samples": [0, 4, 8],
      "depends_on_events": ["evt-001"],
      "consumed_offsets": [0],
      "content_hash": "sha256-hex"
    }
  ]
}
```

客户端发送后把本地包与回显按 `(channel, seq)` 对齐：

- 超时前无回显：`E_MISS`
- 同一包多次回显：`E_DUP`
- 回显时间晚于窗口：`E_TIMEOUT`
- 回显采样不同：`E_DIFF`

异常都带包序号、通道和首次出现时间。诊断界面同时展示干净回显与故障注入回显，故障演示固定注入一个缺包、一个重发、一个超时。

## 配置

| 字段 | 含义 |
|---|---|
| `seed` | 本地确定性生成器种子 |
| `device_port` | 模拟设备回环端口，`0` 为临时端口 |
| `ui_port` | 诊断界面回环端口，`0` 为临时端口 |
| `db_path` | SQLite 文件路径 |
| `sample_dir` | 生成样本目录 |
| `selected_branch` | 运行时选用的分支 |
| `commit_window_packets` | 每窗口提交包数 |
| `workers` | 通道合成并行度上限 |
| `buffer_per_worker` | 每工作线程队列缓冲 |
| `device_timeout_ms` | 回显超时 |
| `param_version` | 能力/请求参数版本 |
| `algorithm_version` | 合成算法版本 |

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_VERSION` | 未知分支、必填字段缺失、能力清单非法 |
| `E_OVER` | tick/毫秒、强度/持续数或合成乘法溢出 |
| `E_PACK` | 包长度、时序窗口、包序连续性非法 |
| `E_STATE` | 非法状态迁移或窗口顺序非法 |
| `E_DIFF` | 三路径/真值不一致或回显采样不一致 |
| `E_MISS` | 缺包 |
| `E_DUP` | 重发 |
| `E_TIMEOUT` | 回显超时 |
| `E_CANCEL` | 主动取消 |
| `E_TERMINATED` | 强制终止 |
| `E_SNAPSHOT` | 快照缺失、版本或哈希不匹配 |

所有结构化错误同时包含分支、字段路径、事件编号、通道、包序号、采样偏移和首次出现时间中适用的上下文。

## 代码结构

```text
cmd/audioforge/          命令行入口、配置、诊断/UI 启动
internal/config/         本地配置
internal/generator/      固定种子样本、能力清单、独立真值
internal/semantics/      三分支严格加载、语义映射、往返编码
internal/num/            溢出检查的整数换算与取整
internal/synth/          通道事件消解、包络叠加、定点量化
internal/packet/         确定性切包、线格式、哈希和校验
internal/deps/           包依赖图与精确失效传播
internal/machine/        包状态机
internal/store/          SQLite 窗口检查点、校验、整快照作废
internal/engine/         实时/恢复/重放、提交、首差异比对
internal/device/         回环模拟设备、客户端、回显对齐
internal/app/            启动诊断编排与报告
internal/web/            内嵌诊断界面和 JSON API
internal/verify/         生成器真值验收
```

## 实际验证结果

本机环境：Go `1.22.12`，Linux amd64，`CGO_ENABLED=0`。

```text
$ CGO_ENABLED=0 go test -timeout 90s ./...
ok  audioforge/internal/app
ok  audioforge/internal/deps
ok  audioforge/internal/device
ok  audioforge/internal/engine
ok  audioforge/internal/generator
ok  audioforge/internal/machine
ok  audioforge/internal/num
ok  audioforge/internal/packet
ok  audioforge/internal/semantics
ok  audioforge/internal/store
ok  audioforge/internal/synth
ok  audioforge/internal/web
```

`CGO_ENABLED=0 go run ./cmd/audioforge -diagnose` 实测结果：

- 3 个分支均归一化为 14 个等价内部事件。
- 生成 3 个通道、27 个音频包、7 个提交窗口。
- 真值验证：81 个采样点、27 个包区间、14 个条件事件判定全部匹配。
- 实时合成、断点恢复、持久化重放三路径帧、包边界和提交序列一致。
- 优先级编辑示例复用 19/27 个缓存包，仅失效同通道受影响的 8 个包。
- 干净回环回显全部 27 个包对齐。
- 故障注入精确得到 1 个 `E_MISS`、1 个 `E_DUP`、1 个 `E_TIMEOUT`。
- 强制终止码为 `E_TERMINATED`，恢复只使用已提交窗口。

诊断界面会展示上述各通道包序列、包采样、回显对齐、异常明细、失效清单、提交窗口和事件决策。
