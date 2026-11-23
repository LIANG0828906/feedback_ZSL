# StageCue Workbench

面向舞台演出与互动媒体场景的确定性灯光演出控制工作台。项目同时提供可集成的 Go 领域组件、本地确定性样本生成器、SQLite 检查点、三路径一致性校验，以及基于 Wails 的桌面诊断界面。

核心约束是：**输出只由工程内容与逻辑时间决定**。工作线程数量、任务完成顺序、SQLite 恢复位置和遍历顺序都不会改变逐帧灯光结果、变化点消费序列或窗口提交序列。

## 功能

- 固定种子生成 `tiny / small / medium / large / stress` 五档工程，最多 50,000+ 变化点、120 盏灯。
- 完整结构校验：时间戳、引用、参数范围、分组环、真值总时长、同刻不可裁决冲突。
- 逐字段插值与 RGB/亮度/色温/频闪渲染，支持 `linear / step / ease_in_out`。
- 基于变化点数、灯具数、帧数与预览 FPS 预算自动选择精确或窗口合并近似模式。
- 分组优先级裁决：单灯覆盖分组；分组按优先级，再按 group id 确定性排序。
- 窗口状态机：`pending → scheduling → committed / failed`，失败可重试。
- SQLite 单写事务检查点，内容哈希、参数版本、算法版本不匹配时整体作废。
- 实时、断点恢复、全量重算三路径逐帧比对，首个差异返回 `E_DIVERGE`。
- 受限并行槽位、总内存预算、每线程缓冲、按窗口顺序的公平排队与阻塞原因记录。
- 工作台展示舞台布局、当前帧 RGB、时间轴、分组裁决、窗口状态、合并段与预算日志。
- 编辑变化点或分组后显式重定基/作废检查点；不会静默改写已提交窗口输出。

## 快速开始

### Windows 10+

```powershell
go version   # 需要 1.22+
go env -w GOOS=windows
go env -w CGO_ENABLED=1

go install github.com/wailsapp/wails/v2/cmd/wails@latest
$env:Path += ";" + (go env GOPATH) + "\bin"

go get modernc.org/sqlite
go mod tidy
```

仓库已锁定兼容 Go 1.22 的 `modernc.org/sqlite v1.34.5`，无需安装系统 SQLite。

### 生成并验证本地样本

```bash
go run ./cmd/stagecue generate --preset small --seed 42 --out samples/small-42
go run ./cmd/stagecue validate --project samples/small-42/project.json

go run ./cmd/stagecue verify \
  --project samples/small-42/project.json \
  --workers 3 \
  --buffer 250000 \
  --memory 300000 \
  --work-dir /tmp/verify-small
```

仓库内已包含：

- `samples/tiny-42/`：54 个变化点、6 盏灯、24 个真值样本。
- `samples/small-42/`：692 个变化点、16 盏灯、64 个真值样本。

所有更大规模样本可由固定种子在本地重新生成，不访问网络资源。

```bash
# 自动取舍，large 会进入 merged 模式
go run ./cmd/stagecue generate --preset large --seed 42 --out /tmp/show-large
go run ./cmd/stagecue run --project /tmp/show-large/project.json --db /tmp/show-large.db
```

### 启动 Wails 工作台

```bash
wails dev
# 或
wails build
```

本项目默认入口即 Wails。`main_headless.go` 提供 `headless` 构建标签，供没有桌面链接库的 CI 使用：`go test -tags headless ./...`。Linux 无显示容器中已验证默认 Go 桌面目标可完成编译；Wails CLI 也成功完成前端和绑定生成，但本沙箱缺少 `pkg-config/GTK` 系统开发包，原生 Wails 链接阶段无法在该沙箱完成。Windows 按上方前置条件构建。

也可以不使用 Wails，直接运行完整 CLI：

```bash
go run ./cmd/stagecue --help
```

### 作为 Go 领域组件

`pkg/stage` 与 `pkg/generator` 不是 `internal` 包，可直接被其他 Go 模块集成：

```go
import (
    "context"
    "stageworkbench/pkg/generator"
    "stageworkbench/pkg/stage"
)

project, err := generator.Generate(generator.Options{Preset: "small", Seed: 42})
if err != nil {
    return err
}
params := stage.DefaultParams().WithDefaults(project)
runner, err := stage.NewRunner(project, params)
if err != nil {
    return err
}
result, err := runner.Run(context.Background(), stage.RunOptions{
    Mode:           stage.RunRealtime,
    CheckpointPath: "show.db",
})
```

若只需要单帧输出，可构造 `stage.NewEvaluator` 后调用 `Frame(frame)`；若需要回归验收，可调用 `stage.VerifyThreePaths`。

## 领域语义

### 工程与变化点

每盏灯包含布局、基础 RGB/亮度/Pan/Tilt/Focus/色温/频闪值，以及逐字段声明范围。变化点按 `time_ms` 单调不减排列，目标必须是单灯或分组之一，并携带稀疏的目标字段、过渡时长、缓动、曲线幂和优先级。

字段插值在逻辑时间上计算：

- `step`：变化点时刻直接取目标值。
- `linear`：从变化点时刻的状态线性过渡到目标。
- `ease_in_out`：`smoothstep`，再应用 `curve_power`。
- 过渡重叠时，后一个变化点从其逻辑时刻的当前插值状态继续。

渲染输出为：

```text
effective_intensity = intensity × strobe_gate
rgb_output           = rgb × effective_intensity × color_temperature_tint
```

所有数值在输出前按 6 位小数确定性归一，比较时不受线程调度影响。

### 优先级与裁决

同一逻辑时刻、同一灯、同一字段：

1. 单灯变化点覆盖任意分组。
2. 多个分组先比 `Group.Priority`，高者胜。
3. 同优先级再比较 cue 优先级。
4. 仍相同则 group id 字典序较小者胜。
5. 同一单灯或同一分组、同一优先级、不同目标值且无法由业务规则继续排序时返回 `E_CONFLICT`，并列出两侧 cue、取值和字段。

每次发生同刻竞争都会记录 `single_overrides_group` 或 `group_priority_then_group_id` 裁决明细。分组父子关系有环时在加载阶段返回 `E_CYCLE`。

### 精确与合并模式

自动取舍依据：

```text
estimated_ops = cue_count × light_count + frame_count × light_count
preview_capacity = (duration_seconds × preview_fps + 1) × light_count × 8
```

超过精确变化点上限、总操作预算或预览 FPS 容量时选择 `merged`。合并模式按预览步长生成关键帧，并强制包含：

- 每个窗口的首尾帧；
- 变化点帧及其前一帧；
- 真值关键采样帧。

每个合并段记录覆盖的 cue 索引/ID 范围、关键帧、误差上界和选用依据。关键采样若超出声明容差返回 `E_SCALE`，定位到时刻、灯具和字段。

### 窗口与检查点

逻辑窗口只允许以下迁移：

```text
pending → scheduling → committed
                   ↘ failed → scheduling(重试)
```

窗口内 cue offset 未完全推进时拒绝提交，非法迁移返回 `E_STATE` 且保持原状态。提交时冻结灯光输出、cue offset、分组快照和合并信息。

SQLite 检查点要求：

- 单连接、互斥写、每窗口一个事务；
- 窗口号必须连续提交；
- 每次写入内容哈希、参数版本、算法版本；
- 重启时任一版本/哈希不匹配则删除整个工程的检查点并全量重算；
- 编辑只允许发生在已提交时间边界之后，且应用层显式执行检查点版本重定基；否则返回 `E_COMMITTED`。

### 并发与内存

- 最大并行度由 `max_workers` 限制。
- 未提交窗口数不超过并行槽位，避免后续窗口抢占资源饿死最早窗口。
- 每个任务按帧数 × 灯具数估算内存，使用总内存预算和每线程缓冲双重限制。
- 内存不足时阻塞并记录 `blocked`，包含窗口号、所需/可用字节、等待时间和原因。
- 内存与调度启动都按窗口号公平排序，检查点写入固定由提交循环串行执行。
- 单窗口异常只标记该窗口 `failed`，已提交窗口不污染；恢复时从连续提交边界续跑。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、时间戳逆序、结构不完整 |
| `E_REF` | 灯具/分组/场景/真值引用不存在 |
| `E_CYCLE` | 分组父子关系成环 |
| `E_PARA` | 参数或插值参数越界，带字段名 |
| `E_SCALE` | 精确/合并关键采样超差，带时刻与灯具 |
| `E_CONFLICT` | 同刻冲突无法确定性裁决 |
| `E_STATE` | 非法窗口迁移、提交不完整、资源状态错误 |
| `E_DIVERGE` | 三路径不一致，带帧、灯具、字段 |
| `E_COMMITTED` | 编辑触及已提交窗口 |
| `E_STORAGE` | SQLite 打开、事务或序列化错误 |

## 组件布局

```text
pkg/stage/            可集成模型、校验、求值器、分组裁决、窗口状态、资源池、SQLite
pkg/generator/        可集成固定种子生成器 + 独立参考真值求值器
cmd/stagecue/         generate / validate / run / verify CLI
app.go                Wails 绑定服务与编辑/恢复 API
main.go               Wails 桌面入口（默认构建）
main_headless.go      headless 构建入口
frontend/dist/        无构建依赖的静态诊断工作台
frontend/wailsjs/     Wails CLI 生成的运行时与绑定声明
samples/              固定种子 tiny/small 工程与真值
docs/                 架构与验证记录
```

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o /tmp/stagecue-workbench .
```

实际验证结果见 [docs/VALIDATION.md](docs/VALIDATION.md)。最近一次本地结果：

- small / exact：1,801 帧、31 窗口、90 次预算阻塞，三路径逐帧一致。
- small / merged：1,801 帧、31 窗口、91 次预算阻塞，三路径逐帧一致。
- large / 自动 merged：34,614 个变化点、80 盏灯、7,201 帧、25 窗口，三路径逐帧一致。
- `go test -race ./...` 全部通过。

本工作区未使用 Docker、容器或外部服务。
