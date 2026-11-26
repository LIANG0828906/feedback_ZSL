# 实际验证记录

验证时间：2026-11-27（Asia/Shanghai）

环境：

- WSL Ubuntu，Linux/amd64
- `go version go1.22.12 linux/amd64`（`go.mod` 声明 `go 1.21`）
- 仅使用 Go 标准库；SQLite 适配器通过 CGO/dlopen 调用环境已有的 `libsqlite3.so.0`

## 自动化测试

命令：

```bash
gofmt -w .
go test ./...
go test -race ./...
CGO_ENABLED=0 go test ./...
go vet ./...
```

实际结果：全部通过。覆盖：

- 三版本装载、未知版本/缺字段 `E_SCHEMA` 字段路径；
- v1 默认通道次序/量化步长补齐；
- v1 降级 `E_LOSSY` 清单和读回语义等价；
- 千分比/万分比、块坐标、通道偏移、溢出 `E_OVF`；
- 三路径逐块逐像素和提交序列一致；
- 块级编辑 21 块复用、1 块重算；
- 失败块重试且窗口恢复；
- worker 预算、像素缓冲上限和排队等待事件；
- 状态机非法迁移保持原状态；
- 依赖清单枚举顺序无关；
- `E_THR`、`E_REGION`、`E_TOP`；
- 真实 SQLite 阶段事务读写。

## 本地确定性样本与 CLI 验证

命令：

```bash
go run ./cmd/iconflow verify
```

实际输出：

```text
samples=samples/seed-42 blocks=22 schema_loads=3 consistency=true checkpoint=sqlite stages=110 windows=3 queue_waits=2 report=artifacts/verification-report.json
```

`queue_waits` 的具体次数会随 worker 调度略有变化；小队列预算下至少会记录 `pending_block_queue_budget_exhausted` 事件和纳秒/毫秒等待时长。

生成内容：

- `samples/seed-42/project.v1.json`
- `samples/seed-42/project.v2.json`
- `samples/seed-42/project.v3.json`
- `samples/seed-42/truth.json`
- `samples/seed-42/dirty-blocks.json`
- `samples/seed-42/images/*.png`
- `artifacts/verification-report.json`
- `artifacts/iconflow.sqlite`

报告摘要：

```text
seed 42042
blocks 22
checkpoint_backend sqlite
consistency_passed True
loss_items 2
sqlite_stages 110
sqlite_windows 3
queue_events 2
schemas:
  icon-project.v1 loaded=true blocks=22 traces=4
  icon-project.v2 loaded=true blocks=22 traces=3
  icon-project.v3 loaded=true blocks=22 traces=3
dirty manifest: 3 blocks, 17 items
```

## SQLite 文件外部核验

命令：

```bash
python3 - <<'PY'
import sqlite3
c=sqlite3.connect('artifacts/iconflow.sqlite')
print(c.execute('select count(*), count(distinct block_key) from stage_commits').fetchone())
print(c.execute('select stage,count(*) from stage_commits group by stage order by stage').fetchall())
print(c.execute('select status,count(*) from window_commits group by status').fetchall())
PY
```

实际结果：

```text
(110, 22)
[('cleaned_stats', 22), ('gradient', 22), ('loaded', 22), ('marked', 22), ('threshold', 22)]
[('committed', 3)]
```

`file artifacts/iconflow.sqlite` 识别为 `SQLite 3.x database`。

## 诊断工作台

启动：

```bash
go run ./cmd/iconflow serve -addr 127.0.0.1:8088 -sqlite artifacts/iconflow.sqlite
```

已实测接口：

- `GET /api/overview`：三版本均装载成功；v1 有 1 个排列损失和 1 个色差容差损失；删除可选字段后产生 2 条默认补齐记录。
- `GET /api/consistency?workers=3&queue=1&buffer=1024`：`ok=true`，提交记录 `110` 条。
- `GET /api/invalidate?field=threshold_scale&scope=block`：影响清单 1 个目标块；二次运行 `21` 块复用、仅目标块重算。
- `GET /api/invalidate?field=channel_order&scope=global`：22 块全部进入失效/重算集合。
- `GET /api/checkpoint?path=artifacts/iconflow.sqlite`：后端 `sqlite`，110 条阶段记录、3 条窗口记录。

页面根路径 `/` 返回 HTTP 200，可查看逐块掩码、阈值、统计、依赖路径和检查点 JSON。
