# 实际验证记录

环境：

- 日期：2026-11-24（Asia/Shanghai）
- 主机工作区：Linux x86_64 容器/沙箱，Go `1.22.12`
- SQLite：`modernc.org/sqlite v1.34.5`（纯 Go）
- Wails：`github.com/wailsapp/wails/v2 v2.9.2`
- 未使用 Docker、外部服务或线上样本

## 自动化测试

```text
go test ./...
ok stageworkbench
ok stageworkbench/pkg/generator
ok stageworkbench/pkg/stage

go test -race ./...
ok stageworkbench
ok stageworkbench/pkg/generator
ok stageworkbench/pkg/stage

go vet ./...
# 无输出，退出码 0

go build -o /tmp/stagecue-workbench .
# 成功，约 10 MB

wails build
# 成功生成 frontend/wailsjs 与 models 绑定
# 原生链接阶段在本 Linux 沙箱因缺少 pkg-config/GTK 开发包退出
```

覆盖内容：

- 生成真值与精确求值器逐样本一致；
- 灯具/分组/同刻 cue 重排后内容哈希与逐帧输出不变；
- `E_SCHEMA / E_REF / E_CYCLE / E_PARA / E_CONFLICT` 定位；
- 状态机非法迁移保持原状态、未消费 cue 拒绝提交、提交结果冻结；
- 单线程与四线程逐帧输出和提交序列一致；
- 检查点连续提交、哈希不匹配整体作废；
- 窗口故障注入后只保留已提交窗口，恢复结果与全量重算一致；
- `E_DIVERGE` 首个帧号、灯具、字段定位；
- `E_SCALE` 时刻、灯具、字段定位；
- 小内存预算下出现阻塞并在释放后完成；
- 单窗口超过每线程缓冲时返回 `E_STATE` 并标记窗口失败；
- 大预设自动选择 `merged`；
- 编辑已提交 cue 返回 `E_COMMITTED`；编辑未来 cue 触发版本重定基，续跑与全量重算一致；
- Wails 服务层与默认 Go 桌面构建；Wails CLI 绑定生成成功。
- 独立临时 Go 模块通过 `replace` 导入 `stageworkbench/pkg/generator` 与 `stageworkbench/pkg/stage`，成功生成工程并求值第 0 帧。

## small / exact

命令：

```bash
go run ./cmd/stagecue verify \
  --project samples/small-42/project.json \
  --workers 2 --buffer 250000 --memory 300000 \
  --force-mode exact --work-dir /tmp/verify-small-exact
```

结果：

```text
consistent=true frames=1801 windows=31 blocked=90 mode=exact
```

三个路径的提交序列均为 `0..30`。90 次 `blocked` 记录包含所需字节、可用字节、总预算和原因；全部最终 `acquired` 并完成。

## small / merged

命令：

```bash
go run ./cmd/stagecue verify \
  --project samples/small-42/project.json \
  --workers 3 --buffer 250000 --memory 300000 \
  --force-mode merged --work-dir /tmp/verify-small-merged
```

结果：

```text
consistent=true frames=1801 windows=31 blocked=91 mode=merged
```

合并关键采样与精确模式一致；预算队列按窗口号公平推进，没有出现后序窗口饿死最早窗口。

## large / 自动取舍

样本由本地固定种子现场生成：

```bash
go run ./cmd/stagecue generate --preset large --seed 42 --out /tmp/sample-large
```

规模：34,614 cue、80 灯、7,201 帧、25 窗口。

自动模式决策：

```text
mode=merged
estimated_ops=3345200
preview_capacity=1843840
reasons:
  cue_count exceeds exact cue limit
  estimated_ops exceeds exact operation budget
  estimated_ops exceeds preview_fps capacity
```

单路径实时调度：

```text
progress=25/25
commit_order=0..24
elapsed≈7.52s
```

三路径验证：

```text
consistent=true frames=7201 windows=25 blocked=0 mode=merged
realtime≈7.52s
recovery≈11.47s
full≈6.99s
```

## 说明

- Linux 沙箱未提供显示器，也未安装 `pkg-config/GTK` 开发包，因此 Wails 窗口未做人工可视化点击验收，Wails CLI 原生链接阶段未完成；绑定生成、前端资源 embed、服务层与默认 Go 桌面目标均已完成编译检查。Windows 10+ 按 README 的 WebView2/CGO 前置条件执行 `wails build`。
- `samples/` 内提交了 tiny 与 small；medium/large/stress 可由固定种子在数秒内重新生成。
