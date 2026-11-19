# etcd 批次结算互斥协调器

本工作区实现一套面向 3 个结算工作进程（A/B/C）的 etcd 租约锁协调能力。etcd 3.5
单节点是锁与租约的唯一事实来源；协调服务提供 gRPC 锁接口、FIFO 等待队列、幂等成果提交、
Prometheus 指标和 JSON 状态接口；worker 以确定性任务数据计算 20 个批次，每个批次 100 条，
最终向本地共享 SQLite 写入 20 个批次、2000 条结算成果。

## 已验证结果

- 3 worker、20 批次、2000 条任务：正常场景和强制终止 worker 场景均完成。
- 故障演练中强杀持有 `batch-009` 的 B：该批次租约到期后记录 `lease_expired`，
  其余 worker 继续推进，B 重启后完成剩余批次。
- 最终协调状态、SQLite 成果和确定性摘要一致：20 个批次、2000 条任务行、每批恰好一个规范成果。
- 实际 Windows x64 验收（含强制终止 B）：16.3 秒完成，3 个 worker 峰值工作集合计 52.8 MiB。
- 探针覆盖互斥、同持有者续约、`E_LOCK_HELD`、`E_WAIT_TIMEOUT`、
  `E_LEASE_TOO_SHORT`、过期释放、FIFO、重复提交和 `E_LOCK_LOST`。
- 最新机器可读结果见 `artifacts/lock-probe-report.json` 与
  `artifacts/acceptance-summary.json`。

## 目录

```text
cmd/coordinator/         gRPC 协调服务与 HTTP 状态/指标入口
cmd/worker/              独立结算工作进程
cmd/taskgen/             固定种子确定性任务生成器
cmd/probe/               锁语义验收探针
cmd/verify/              最终状态 + SQLite 一致性校验器
internal/coordinator/    etcd 锁、事务、FIFO、看门狗、指标
internal/worker/         领取、续约、计算、提交、SQLite 落地
internal/storage/        SQLite schema 与原子/幂等写入
internal/tasks/          确定性生成与摘要
internal/api/            protobuf 定义与生成代码
configs/                 coordinator 与 A/B/C 示例配置
data/                    2000 条任务数据及 manifest
etcd/                    etcd 3.5.21 Windows x64 可执行文件
build-win/               已交叉构建的 Windows x64 可执行文件
scripts/                 构建、启动、停止和验收脚本
artifacts/               验收报告、指标快照和状态快照
```

## Windows 快速启动

环境要求：Windows 10/11 或 Windows Server 2022、Go 1.22、本工作区内的
`etcd\etcd.exe`。所有监听地址均为 `127.0.0.1`。

```powershell
# 1. 可选：重新测试并构建
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1

# 2. 生成 2000 条任务（已提供数据，可重复生成）
.\build-win\taskgen.exe -out .\data

# 3. 启动 etcd、协调服务和 A/B/C
powershell -ExecutionPolicy Bypass -File .\scripts\start.ps1 -ResetEtcd

# 4. 查看状态
Invoke-RestMethod http://127.0.0.1:19090/v1/status/batch-001
Invoke-WebRequest http://127.0.0.1:19090/metrics

# 5. 停止
powershell -ExecutionPolicy Bypass -File .\scripts\stop.ps1
```

协调服务固定使用：

- gRPC：`127.0.0.1:17070`
- HTTP 状态/指标：`127.0.0.1:19090`
- etcd：`127.0.0.1:2379`，peer 为 `127.0.0.1:2380`

端口可在 `configs/coordinator.json` 和三个 worker 配置中修改。

## 一键验收

```powershell
# 正常场景
powershell -ExecutionPolicy Bypass -File .\scripts\acceptance.ps1

# 强制终止持有批次的 B，再重启 B
powershell -ExecutionPolicy Bypass -File .\scripts\acceptance.ps1 -Fault
```

验收脚本会自行启动独立 etcd 数据目录，依次执行：

1. 锁语义探针（独立 etcd key 前缀，不污染结算批次）。
2. 启动 A/B/C，处理 20 个批次。
3. `-Fault` 时监控到 B 持有未提交批次后强杀 B，等待租约过期并重启 B。
4. 校验协调服务状态、SQLite 行数/摘要、Prometheus 指标和 120 秒时限。

Linux/WSL 辅助验收：

```bash
GO_BIN=/path/to/go ETCD_BIN=.run/etcd-linux/etcd bash scripts/acceptance.sh
GO_BIN=/path/to/go ETCD_BIN=.run/etcd-linux/etcd bash scripts/acceptance.sh --fault
```

## gRPC 接口

协议定义：`internal/api/coordinator.proto`。

业务错误通过响应中的 `error_code` 返回，gRPC 状态保持 `OK`；只有网络、etcd
不可用等基础设施错误使用非 OK gRPC 状态。

| 接口 | 行为 |
|---|---|
| `AcquireLock` | 申请批次锁；同持有者重复申请等同续约；支持 FIFO 等待 |
| `RenewLock` | 刷新租约到期时间；租约过短时拒绝且不改变已有锁 |
| `ReleaseLock` | 正常释放并记录 `released` 事件 |
| `SubmitResult` | 原子提交规范成果；重复提交返回首次成果 |
| `GetStatus` | 返回指定批次持有者、租约、成果和释放事件 |
| `ListStatus` | 返回全部已知结算批次状态 |

稳定业务错误码：

| 错误码 | 含义 |
|---|---|
| `E_LOCK_HELD` | 批次已被其他持有者占用；响应同时给出 `current_holder_id` |
| `E_LOCK_LOST` | 调用者已不再持有锁；提交不会写任何部分成果 |
| `E_WAIT_TIMEOUT` | FIFO 等待超过请求的 `wait_timeout_ms` |
| `E_LEASE_TOO_SHORT` | 请求租约低于 3000 ms；既有锁保持不变 |
| `E_INVALID_ARGUMENT` | 参数非法 |
| `E_INTERNAL` | 服务内部原子提交失败 |

`AcquireLockRequest.lease_ttl_ms = 0` 使用协调服务默认的 6000 ms。
`wait_timeout_ms = 0` 表示只尝试一次；正数表示按 FIFO 等待的毫秒数。

### 租约与过期

- 最小租约 3000 ms，默认 6000 ms；worker 默认每 2000 ms 续约一次。
- etcd 租约是最终过期机制；协调服务每 150 ms 巡检，并把过期锁主动收敛为可申请状态。
- 过期后 1 秒内新申请可成功；验收探针直接检查该时间上界。
- 锁删除后记录释放事件，字段包含原持有者、批次、租约到期时间、释放时间、generation、
  lease id 和稳定原因 `released`、`lease_expired`、`lock_missing`。
- worker 在提交前同时检查续约状态和本地到期时间，避免 GC 停顿后提交已经失去锁的成果。

### 成果幂等与 SQLite

- etcd 中每个批次只有一个规范成果键 `/settlement/v1/results/<batch>`。
- 第一次成功提交写入规范成果；同持有者重复提交或新持有者重新提交都返回首次成果。
- 响应中 `accepted=true` 表示请求处理成功；`idempotent=true` 表示没有产生新的规范提交。
- `committed_by` 始终是首次成功提交的持有者。
- SQLite 仅在协调服务接受成果后写入，且一个批次和它的 100 条明细在同一事务中提交。
- 提交者失锁时返回 `E_LOCK_LOST`，不会写 SQLite 或 etcd 部分结果。
- 状态接口携带规范 `result_json`；worker 在退出前会补齐共享 SQLite 中缺失的已提交批次。

SQLite 文件：`data/settlement.db`（一键验收为 `data/acceptance.db`）。

## HTTP 状态与指标

所有接口均为 GET：

| 路径 | 内容 |
|---|---|
| `/healthz` | etcd 连通性 |
| `/v1/status/{batch_id}` | 指定批次 JSON 状态 |
| `/v1/locks/{batch_id}` | 指定批次 JSON 状态别名 |
| `/v1/status?batch_id=...` | 指定批次 JSON 状态 |
| `/v1/status` | 全部批次状态 |
| `/metrics` | Prometheus 文本格式 |

稳定指标名（所有锁指标均带稳定 `batch` 标签）：

```text
settlement_lock_held
settlement_lock_owner_info{batch,holder}
settlement_lock_lease_remaining_seconds
settlement_lock_acquire_requests_total
settlement_lock_renew_requests_total
settlement_lock_release_requests_total
settlement_result_submissions_total
settlement_lock_release_events_total{batch,reason}
settlement_lock_wait_timeouts_total
```

`settlement_lock_owner_info` 在无持有者时输出 `holder=""` 且值为 0。

## 配置

- `configs/coordinator.json`：监听地址、etcd 地址、key 前缀、默认/最小租约、巡检频率、
  批次数量与 Prometheus 批次标签生成规则。
- `configs/worker-A.json`、`worker-B.json`、`worker-C.json`：进程标识、gRPC 地址、共享
  SQLite 路径、任务文件、租约/续约/等待/计算耗时。
- worker 启动时必须满足 `renew_interval_ms > 0` 且 `renew_interval_ms * 2 < lease_ttl_ms`。
- 协调服务启动时拒绝小于 3000 ms 的最小租约配置；运行期拒绝过短申请/续约且不改变锁状态。

任务生成：

```powershell
.\build-win\taskgen.exe -out .\data -seed 28120261119 -batches 20 -tasks-per-batch 100
```

生成器保证相同种子产生完全相同的 2000 条任务；`data/manifest.json` 固定记录批次和数量。
成果摘要只包含批次、任务号和金额，不包含 holder，因此故障后换持有者重算仍得到相同摘要。

## 资源控制

- etcd 仅启动一个单节点实例，复用本机 `127.0.0.1:2379`。
- 验收/启动脚本为协调服务和 3 个 worker 设置 `GOMAXPROCS=1`；单批数据量为 100 条，
  不启动数据库或消息系统额外实例。
- worker 共享一个 SQLite 文件，WAL 模式 + 30 秒 busy timeout + 事务级 BUSY 重试。
- 2GB 临时空间约束内只保留 etcd、SQLite/WAL、少量日志和验收 JSON。

更详细的一致性设计见 `docs/DESIGN.md`。
