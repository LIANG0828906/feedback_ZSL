# 双环配置中心与客户端模拟器

这是一个可直接在 Windows Server 2022 / Windows 10 x64 或 Linux 上构建运行的 Go 实现。配置中心是单个轻量进程，配置、版本、发布、审计和客户端环归属均持久化到本地 SQLite；模拟客户端也是单个独立可执行文件，用 24 个 goroutine 模拟 24 个实例。

## 核心设计

- **不可变快照**：每个发布环使用一条完整配置快照。全量环始终读取 `namespace_heads` 指向的已发布快照；灰度环在有观察任务时读取候选快照。两者不会混读。
- **CAS 版本控制**：每个配置项有独立递增 `item_version`。修改必须携带 `expected_version`，不匹配返回业务码 `E_CONFLICT`。
- **保留历史**：每次变更写入 `config_item_versions`，默认保留每项最近 20 个版本（最低配置下限为 5）。
- **双环状态机**：`draft -> canary -> observed_pass -> full`；`canary/observed_pass -> rolled_back`。观察期内强制推进返回 `E_OBSERVATION_ACTIVE`，其他非法迁移返回 `E_INVALID_TRANSITION` 且状态不变。
- **自动回滚**：服务端后台观察器汇总灰度实例的累计成功/错误计数。观察结束时错误率 `> 0.005` 自动回滚并审计；否则进入 `observed_pass`，等待人工推进全量。
- **原子发布**：全量推进在同一个 SQLite 事务中完成配置项替换、逐项历史插入、逐项审计、发布审计和 head 指针切换。任一步失败都不会留下部分发布结果。
- **客户端兜底**：模拟客户端在独立 SQLite 中保存最近一次成功快照。启动时先恢复快照；轮询遇到注入的 5xx 或网络不可达时继续使用快照，并在恢复后的上报中携带 `local_version` 和 `offline`。
- **读取一致性**：查询接口在 SQLite 只读事务内先读取 head 快照 ID，再读取该不可变快照的全部条目。并发写入不会产生部分更新视图。

## 目录

```text
cmd/configd/           配置中心
cmd/simclient/         独立模拟客户端与 JSON 报告
cmd/scenario/          双环发布驱动与端到端结果校验
internal/model/        协议、业务码、快照摘要
internal/store/        SQLite 模型、CAS、发布状态机、审计
internal/server/       HTTP API 与观察调度
internal/clientstate/  客户端本地快照 SQLite
scripts/e2e.sh         Linux/WSL 端到端脚本
scripts/e2e.ps1        Windows PowerShell 端到端脚本
docs/API.md            API 与状态机说明
docs/REPORT.md         模拟报告字段与复现参数
```

## 构建与测试

要求 Go 1.22+。SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，无需 CGO 或数据库服务。

```bash
go mod download
go test ./...
go build -trimpath -o bin/configd ./cmd/configd
go build -trimpath -o bin/simclient ./cmd/simclient
go build -trimpath -o bin/scenario ./cmd/scenario
```

Windows 下将输出文件改为 `.exe` 即可：

```powershell
go build -trimpath -o bin\configd.exe .\cmd\configd
go build -trimpath -o bin\simclient.exe .\cmd\simclient
go build -trimpath -o bin\scenario.exe .\cmd\scenario
```

## 本地运行

启动配置中心并初始化 300 个参数：

```bash
GOMAXPROCS=1 GOMEMLIMIT=768MiB ./bin/configd \
  -addr 127.0.0.1:8080 \
  -db data/config.db \
  -log logs/configd.log \
  -seed 300 \
  -seed-namespace payments \
  -canary-percent 10 \
  -observation-seconds 10 \
  -error-threshold 0.005
```

另开终端启动 24 个模拟实例：

```bash
GOMAXPROCS=1 GOMEMLIMIT=384MiB ./bin/simclient \
  -server http://127.0.0.1:8080 \
  -namespace payments \
  -instances 24 \
  -poll 10s \
  -poll-count 6 \
  -duration 90s \
  -seed 263 \
  -fault-5xx-rate 0.03 \
  -network-error-rate 0.02 \
  -latency-probability 0.30 \
  -latency-min 200ms \
  -latency-max 2s \
  -read-workers 6 \
  -read-interval 200ms \
  -state-db data/simclient.db \
  -report reports/simulation-report.json
```

模拟器参数决定负载和故障注入；`-poll-count` 可固定每个实例的拉取次数，报告字段固定，实例按 ID 排序，随机故障由 `-seed` 确定。

## 一键端到端验证

Linux/WSL：

```bash
./scripts/e2e.sh
```

Windows PowerShell：

```powershell
.\scripts\e2e.ps1
```

脚本会构建三个程序、启动配置中心、启动 24 个模拟实例、依次验证：

1. 灰度环读取候选版本，全量环保持旧 head。
2. 观察期内强制推全被拒绝。
3. 正常发布通过观察后推全，两个环最终收敛。
4. 第二次发布注入高生效错误率，超过 0.5% 后自动回滚，全量版本不变。
5. 离线兜底触发、审计轨迹、历史版本、报告字段和 6 并发读取 P95。

最近一次本地运行的产物：

- `reports/e2e-20261118T180533Z/simulation-report.json`
- `reports/e2e-20261118T180533Z/scenario-summary.json`

该次结果：24 个实例、每个实例固定 25 次拉取、灰度 2 个（8.33%）、自动回滚 1 次、离线兜底 29 次、读取一致性探测 1,800 次、P95 3.290 ms。

## 资源约束

- 端到端脚本给配置中心 `GOMAXPROCS=1`、模拟器 `GOMAXPROCS=1`，只消费本机两个逻辑 CPU 的并行执行能力；场景驱动只在等待/校验时短暂运行。
- `GOMEMLIMIT` 分别设置为 768 MiB、384 MiB、64 MiB，合计低于 1.25 GiB。
- Go 构建缓存、临时目录、数据库、日志和报告均位于工作区内；本次运行数据约 8.4 MiB。
- 不启动 Redis、Kafka、外部 SQLite 服务或重复配置中心节点。SQLite 使用单进程 WAL 模式，发布写入由进程内互斥锁串行化，读请求可并发。

更完整的请求/响应示例、业务码和状态迁移规则见 `docs/API.md`。
