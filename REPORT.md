# 归档平台交付与验收报告

日期：2026-11-19（Asia/Shanghai）

## 1. 结论

服务按“客户端分片/对象写入 → 4+2 条带编码 → 6 个块目录 → Redis 元数据 → SQLite 审计/配额 → 备站点修复”的链路交付完成。
本轮验收使用 200 个固定种子对象（305,677,698 字节，380 个条带，2,280 个主站点块）完成：

- 健康读取全量校验通过；
- 两盘下线时 380 个条带全部降级可读，600 次响应重建读取 0 差异；
- 第三盘下线时 380 个条带全部 `STRIPE_UNRECOVERABLE`，读取返回条带与缺失块；
- 双盘修复 380 个条带，380 次校验成功、0 次失败，写回 152,838,884 字节；
- 修复后全量读取通过；主备对账能输出缺失、长度、摘要三类差异；
- 配额耗尽、风险优先、校验失败不提交、服务重启续修、下线/恢复原子性均通过。

## 2. 实际使用项声明

| 项 | 实际使用 |
|---|---|
| 操作系统 | Windows 主机上的 WSL2 Ubuntu 26.04 |
| Go | `go1.22.12 linux/amd64` |
| Redis | `redis-server v=7.2.7`，源码构建，`127.0.0.1:6379` |
| Redis 启动 | `--daemonize yes --save '' --appendonly no`，验收期间保持运行 |
| SQLite | `github.com/mattn/go-sqlite3 v1.14.24`，驱动内置 SQLite，无外部 SQLite 服务 |
| 条带 | 4 数据 + 2 校验，默认条带 1 MiB |
| 并行度 | `runtime.GOMAXPROCS(2)` |
| 修复并发 | 2 |
| 时区 | `Asia/Shanghai`，配额按本地日期滚动 |

最终验收实例：

```text
config/acceptance-final.json
127.0.0.1:18086
Redis DB 6, namespace archive:final
data_root ./var/final
SQLite ./var/final/final.db
writeback_quota_bytes 1073741824
auto_repair false（本轮用手动 drain 控制时点）
```

默认主配置 `config/archive.json` 的额度为 `1 GiB = 1,073,741,824` 字节。若需严格按十进制
1 GB 解释，可将 `writeback_quota_bytes` 设为 `1000000000`；本轮实际写回 152,838,884 字节，
远低于两种口径。

## 3. 链路与一致性设计

### 3.1 写入

1. 按 `stripe_size` 将对象切成逻辑条带。
2. 每个条带补零到 4 个等长数据分片，Reed-Solomon 生成 2 个校验分片。
3. 6 个分片分别写入 `blocks/block-N/<stripe-id>/N.blk`，先写临时文件、`fsync`、再 `rename`。
4. 每个块记录相对路径、长度、SHA-256；条带记录 `offset_start/offset_end`、`length`、
   `shard_length`、逻辑 `data_sha256`、块列表。
5. 所有条带元数据与对象元数据放在同一个 Redis `TxPipeline`（MULTI/EXEC）中登记；
   对象元数据最后可见，避免读到半个对象。

### 3.2 读取

- 先读取对象元数据和所涉条带，再在全局读锁下读取块。
- 每个主块先校验长度与 SHA-256；主块不可用时尝试备站点块。
- 可用块数 < 4：`STRIPE_UNRECOVERABLE`。
- 有缺失/坏块但 ≥ 4：`Reconstruct` 重建，随后重新做 RS `Verify`，并校验逻辑条带 SHA-256。
- 所有涉及条带成功后才写 HTTP 响应；任何条带失败都不会返回部分内容。
- 读取发现主块/备块摘要不符时，会把该块标记为不可用并唤醒修复调度；
  风险队列随后按新的可用块数重新分级。
- 降级读取写入 SQLite 审计，并在 `X-Archive-Degraded`、`X-Archive-Reconstructed-Blocks`
  中暴露给调用方。

### 3.3 下线/恢复原子性

- 进程内 `sync.RWMutex`：读取持读锁贯穿条带读取与重建；`offline/online` 持写锁。
- Redis 目录状态更新使用 Lua 原子执行，并递增 `layout_epoch`。
- 读取结束前再次检查 epoch；跨实例修改导致变化时最多重试 3 次，否则返回 `LAYOUT_CHANGED`。
- 本轮额外做了 8 次 offline/online 切换，同时读取 200 个对象，0 失败、0 摘要差异。

### 3.4 修复与配额事务

- 调度器为每个条带获取 Redis 租约，避免重复修复。
- 修复前按计划缺失块数 × `shard_length` 预留额度。
- SQLite 事务 1：写 `repair_jobs(state=running)` + `quota_ledger(state=reserved)` + 审计。
- 重建后做 RS 校验与逻辑 SHA-256 校验，校验通过才写备站点目录。
- SQLite 事务 2：`repair_jobs(state=completed)` + `quota_ledger(state=committed)` + 校验审计。
- SQLite 提交后再更新 Redis `backup_state=committed`；提交前备站点文件不参与读取。
- 失败时在 SQLite 同一事务中写 `repair_jobs(state=failed)` + `quota_ledger(state=released)` + 失败审计，
  并设置 30 秒冷却。
- 重启时加载 `state=running` 的任务继续；已提交任务不会重复扣额度。

### 3.5 调度规则（可复核）

1. `available_blocks` 升序，越少越先。
2. 同等风险：`last_read_at` 越早越先；从未读取最先。
3. 再按对象大小升序。
4. 最后按 `stripe_id` 升序。
5. 只处理当前最高风险组；该组存在因额度不足/冷却未启动的条带时，不跳到低风险组。

`/v1/repairs/queue` 返回 `priority_reason`、`state`、`skip_reason`，`skipped` 数组列出被跳过条带。

### 3.6 对账口径

- 开始时快照目录在线状态与条带元数据。
- 只比较快照中已提交的备站点写回块（`backup_state=committed`）。
- 开始时下线的块目录整体排除。
- 修复中/未提交目标块不报差异。
- 两侧存在时比较长度与 SHA-256，分别输出 `missing`、`length_mismatch`、`digest_mismatch`。
- 文件读取不加全局读锁，允许并发写入/读取/修复；结论只使用开始快照。

## 4. 主验收结果

### 4.1 固定数据集

来自 `artifacts/final/seed.json`：

| 指标 | 值 |
|---|---:|
| 对象数 | 200 |
| 小对象（64 KiB..512 KiB） | 140 |
| 中等对象（1 MiB..4 MiB） | 60 |
| 对象总字节 | 305,677,698 |
| 对象总 MiB | 291.52 |
| 写入条带数 | 380 |
| 主站点块数 | 2,280 |
| 主站点块字节 | 458,516,652 |
| 种子写入耗时 | 6.86 s |

### 4.2 健康读取

来自 `artifacts/final/check-healthy.json`：

```json
{
  "objects_checked": 200,
  "full_objects_checked": 200,
  "ranges_checked": 400,
  "bytes_read": 307309993,
  "degraded_responses": 0,
  "failures": 0,
  "unrecoverable_responses": 0,
  "mismatches": []
}
```

`archive-check` 对每个对象读取完整内容，并额外读取 2 个确定性范围；全部与固定种子重算结果一致。
最终二进制重启后再次执行同一校验，结果仍为 200 对象、307,309,993 字节、0 失败
（`artifacts/final/check-post-restart.json`）。

### 4.3 两盘下线：降级读取

`block-0`、`block-1` 下线后：

```text
stripes=380
degraded_stripes=380
unrecoverable_stripes=0
queue_depth=380
available_blocks=4（每个条带）
risk=critical
```

全量校验 `artifacts/final/check-degraded.json`：

```json
{
  "objects_checked": 200,
  "full_objects_checked": 200,
  "ranges_checked": 400,
  "bytes_read": 307309993,
  "degraded_responses": 600,
  "failures": 0,
  "unrecoverable_responses": 0,
  "mismatches": []
}
```

降级响应头示例（`artifacts/final/read-degraded.headers`）：

```text
HTTP/1.1 206 Partial Content
Content-Range: bytes 0-4095/4194304
X-Archive-Degraded: true
X-Archive-Reconstructed-Blocks: 2
X-Archive-Stripes-Touched: 1
```

### 4.4 第三盘下线：不可恢复错误

再下线 `block-2` 后，380 个条带均只有 3 个可用块：

```text
degraded_stripes=0
unrecoverable_stripes=380
```

读取返回（`artifacts/final/read-unrecoverable.json`）：

```json
{
  "error": {
    "code": "STRIPE_UNRECOVERABLE",
    "message": "fewer than four blocks are available",
    "stripe_id": "seed-s-000-s000000",
    "object_id": "seed-s-000",
    "missing_blocks": [0, 1, 2],
    "available_blocks": 3
  }
}
```

`archive-check -mode unrecoverable` 对 200 个对象全部得到稳定错误码，未出现内容返回。

### 4.5 修复与重建校验

恢复 `block-2` 在线，仅保留两盘下线后执行 `POST /v1/repairs/drain`：

```text
started=380
runnable_remaining=0
active_repairs=0
```

配额账本（`artifacts/final/quota-final.json`）：

```json
{
  "day": "2026-11-19",
  "limit_bytes": 1073741824,
  "used_bytes": 152838884,
  "reserved_bytes": 0,
  "committed_bytes": 152838884,
  "remaining_bytes": 920902940
}
```

单条带审计示例（`artifacts/final/audit-sample-repair.json`）：

```text
stripe_id=seed-m-000-s000000, object_id=seed-m-000
repair_job state=completed, reserved_bytes=524288
result.verification=parity_ok+logical_sha256_ok
repaired_blocks=2
quota_rows=[("committed", 524288)]
```

修复后指标：

```text
archive_repair_success_total 380
archive_repair_failure_total 0
archive_degraded_stripes 380   # 两个坏盘仍下线，但全部由已校验备站点块服务
archive_unrecoverable_stripes 0
archive_repairs_in_progress 0
```

修复后全量读取（`artifacts/final/check-after-repair.json`）：

```json
{
  "objects_checked": 200,
  "bytes_read": 307309993,
  "degraded_responses": 600,
  "failures": 0,
  "unrecoverable_responses": 0,
  "mismatches": []
}
```

此后恢复 `block-0/block-1` 在线，降级条带数回到 0。

### 4.6 主备对账

| 场景 | 比较条带 | 比较块 | missing | length | digest |
|---|---:|---:|---:|---:|---:|
| 两盘下线时对账 | 0 | 0 | 0 | 0 | 0 |
| 恢复所有目录后 | 380 | 760 | 0 | 0 | 0 |
| 注入 1 个备块缺失/1 个主块截断/1 个主块字节翻转 | 380 | 760 | 1 | 1 | 1 |
| 修复注入差异后 | 380 | 760 | 0 | 0 | 0 |

三类差异摘要（`artifacts/final/reconcile-diffs.json`）：

```text
missing:
  seed-s-000-s000000 block 0, standby missing
length_mismatch:
  seed-s-001-s000000 block 0, primary=12345 standby=130816
digest_mismatch:
  seed-s-002-s000000 block 1, primary_sha256 != standby_sha256
```

注入差异后全量读取仍通过：

```json
{
  "objects_checked": 200,
  "bytes_read": 307309993,
  "degraded_responses": 6,
  "failures": 0,
  "mismatches": []
}
```

读取对坏主块自动回退到已校验备块，没有返回错误字节。

## 5. 专项验收

### 5.1 配额耗尽

独立实例 `config/quota-final.json`，额度 1,048,576 字节，6 个条带各缺 1 块（每块 262,144 字节）：

```text
started=4
quota used=1048576, remaining=0
waiting_quota=2
```

`artifacts/quota-final/queue-after.json` 中剩余两条带状态为 `waiting_quota`，
`skip_reason=quota_insufficient`；已完成的 4 个条带继续可用，全量读取 0 失败。

### 5.2 风险优先与低风险不挤占

独立实例构造两种风险：

```text
rank 1  risk-a-critical  available_blocks=4  risk=critical  estimated_bytes=50000  runnable
rank 2  risk-b-high      available_blocks=5  risk=high      estimated_bytes=250000 waiting_quota
```

额度 102,400 字节：

- drain 只启动 `risk-a-critical`；
- 完成后额度已用 50,000，剩余 52,400；
- `risk-b-high` 仍为 `waiting_quota`，没有因风险更低而抢占额度；
- 两个对象读取均 0 失败。

产物：`artifacts/risk-final/`。

另在同一风险专项中人为翻转一个主块字节：读取仍返回正确内容（重建 2 块），
同时该主块被标记为不可用，队列从 `available_blocks=5/missing=[0]` 重新分级为
`available_blocks=4/missing=[0,2]/critical`。产物：
`artifacts/risk-final/corrupt-read.bin`、`queue-after-corrupt.json`。

### 5.3 校验失败不提交

独立实例中故意篡改条带 `data_sha256`，模拟重建结果与预期数据范围不符：

```text
job state=failed
reserved_bytes=262144
quota ledger=released
quota used=0
standby block exists=false
repair_failure_total=1
read status=503
read code=STRIPE_INTEGRITY_FAILED
```

该条带保持降级，失败原因写入审计，未把未校验块标记为可用。
产物：`artifacts/fail/`。

### 5.4 重启续修

4 MiB 对象、1 盘下线、修复延迟 3 秒；在两个修复任务处于 `running/reserved` 时 `kill -9`，
随后用相同 SQLite/Redis/数据目录重启：

重启前：

```text
jobs: 2 × running, reserved 262144 each
quota: reserved 524288
```

重启后：

```text
jobs: 2 × completed, metadata_applied=1
quota: committed 524288
completed repair audit rows: 2
reads: 0 failures
```

额度没有被重复消耗。产物：`artifacts/restart-final/`。

### 5.5 下线/恢复与并发读取原子性

对 `block-0` 连续执行 8 次 offline/online，同时全量读取 200 个对象：

```json
{
  "objects_checked": 200,
  "full_objects_checked": 200,
  "ranges_checked": 200,
  "bytes_read": 306491819,
  "degraded_responses": 179,
  "failures": 0,
  "mismatches": []
}
```

没有出现半条带旧目录状态或错误拼接。

### 5.6 原始字节上传与 Range 头

100,000 字节 `PUT` 上传，其中 `block-0` 离线；服务只写 5 个主块，条带降级。
随后用标准 `Range: bytes=12345-23456` 读取：

```text
Content-Range: bytes 12345-23456/100000
X-Archive-Degraded: true
X-Archive-Reconstructed-Blocks: 1
length=11112
SHA-256 与原始切片一致
```

## 6. 资源与并发

`scripts/resource_monitor.py` 在最终验收期间每 50 ms 采样：

```json
{
  "duration_sec": 129.02,
  "samples": 1336,
  "max_repairs_in_progress": 2,
  "max_archive_and_redis_rss_bytes": 125394944,
  "max_archive_and_redis_rss_mib": 119.586,
  "max_matched_processes": 8,
  "rss_scope": "all archive-server and redis-server processes in this WSL2 instance"
}
```

- 修复并发峰值 2，符合上限。
- 所有 `archive-server` + `redis-server` 进程峰值 RSS 约 120 MiB，低于 1.25 GiB。
- 最终数据目录约 603 MiB（主块 2,280 个 + 备块 760 个），低于 2 GB 临时文件上限。
- 服务 `GOMAXPROCS=2`，修复 worker 上限 2。

产物：`artifacts/final/resource-monitor.json`。

## 7. 指标与状态摘要

最终指标（`artifacts/final/metrics-after-atomicity.txt`）：

```text
archive_block_dir_online{dir="block-0"} 1
...
archive_degraded_stripes 0
archive_unrecoverable_stripes 0
archive_writeback_bytes_today 152838884
archive_quota_remaining_bytes_today 920902940
archive_repairs_in_progress 0
archive_repair_success_total 380
archive_repair_failure_total 0
archive_repair_queue_depth 0
archive_reconcile_differences{type="missing"} 0
archive_reconcile_differences{type="length"} 0
archive_reconcile_differences{type="digest"} 0
archive_objects_total 200
archive_stripes_total 380
```

JSON 状态摘要顶层字段顺序：

```text
generated_at
service_started_at
redis_version
object_count
stripe_count
block_dirs
quota
repairs
last_reconciliation
```

`block_dirs` 按 `block-0..block-5` 固定顺序；`quota` 字段固定为
`day/limit_bytes/used_bytes/reserved_bytes/committed_bytes/remaining_bytes`；
状态与队列不包含绝对路径。

## 8. 产物索引

```text
artifacts/final/seed.json                         200 对象固定种子写入
artifacts/final/summary.json                       验收关键指标汇总
artifacts/final/check-healthy.json                健康全量读取校验
artifacts/final/status-two-offline.json           双盘下线状态与风险队列
artifacts/final/check-degraded.json                双盘降级全量读取
artifacts/final/read-degraded.headers              降级读取响应头
artifacts/final/read-unrecoverable.json            三盘下线稳定错误
artifacts/final/check-unrecoverable.json           200 对象不可恢复校验
artifacts/final/repair-queue-before.json           修复前队列
artifacts/final/repair-drain.json                  本轮修复汇总
artifacts/final/quota-final.json                   修复后配额账本
artifacts/final/audit-sample-repair.json           单条带降级/修复/校验历史
artifacts/final/check-after-repair.json            修复后全量读取
artifacts/final/check-post-restart.json            最终二进制重启后全量读取
artifacts/final/reconcile-excluded.json            下线目录排除对账
artifacts/final/reconcile-clean.json               恢复后无损对账
artifacts/final/reconcile-diffs.json               三类差异对账
artifacts/final/check-after-reconcile-corruption.json  坏副本下读取仍正确
artifacts/final/reconcile-healed.json              差异修复后对账
artifacts/final/check-atomicity.json               下线/恢复并发读取
artifacts/final/resource-monitor.json              峰值内存与并发
artifacts/final/status-final.json                  最终 JSON 状态
artifacts/final/metrics-after-atomicity.txt        最终 Prometheus 指标
artifacts/final/status-latest.json                 最终二进制重启后状态
artifacts/final/metrics-latest.txt                 最终二进制重启后指标
artifacts/quota-final/                             配额耗尽专项
artifacts/risk-final/                              风险优先专项
artifacts/risk-final/corrupt-read.bin              坏主块读取仍正确
artifacts/risk-final/queue-after-corrupt.json      坏块标记后风险重新分级
artifacts/fail/                                    校验失败不提交专项
artifacts/restart-final/                           重启续修专项
```

## 9. 复现

完整命令见 `README.md`。核心复现顺序：

```bash
export PATH="$PWD/.tools/go/bin:$PATH"
export GOMAXPROCS=2
go test ./...
go build -o bin/archive-server ./cmd/archive-server
go build -o bin/archive-check  ./cmd/archive-check

# Redis 7.2.7 已在 127.0.0.1:6379 运行
./bin/archive-server -config config/acceptance-final.json

curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/seed \
  -H 'Content-Type: application/json' \
  -d '{"profile":"full","seed_base":20261119,"skip_existing":true}'

./bin/archive-check -addr http://127.0.0.1:18086 -mode healthy -ranges 2 \
  -json artifacts/final/check-healthy.json

curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/dirs/0/offline \
  -H 'Content-Type: application/json' -d '{"reason":"bad_disk_0"}'
curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/dirs/1/offline \
  -H 'Content-Type: application/json' -d '{"reason":"bad_disk_1"}'

./bin/archive-check -addr http://127.0.0.1:18086 -mode degraded -ranges 2 \
  -json artifacts/final/check-degraded.json

curl --noproxy '*' -X POST \
  'http://127.0.0.1:18086/v1/repairs/drain?timeout_sec=240'

curl --noproxy '*' http://127.0.0.1:18086/v1/quota
./bin/archive-check -addr http://127.0.0.1:18086 -mode degraded -ranges 2 \
  -json artifacts/final/check-after-repair.json
```

## 10. 边界说明

- 备站点只保存经校验的修复写回块，不是全量镜像；对账口径严格限定为“已写回备站点块”，
  与需求中的“备站点已写回的块集合”一致。
- 单实例内修复并发由信号量限制为 2；跨实例通过 Redis 条带租约避免同一条带重复修复。
  多实例部署时使用独立 `redis_namespace`/`redis_db`、SQLite 文件和端口隔离。
- 验收配置 `auto_repair=false`，以便固定验收时点；主配置默认 `auto_repair=true`。
- Redis 在验收期间未重启；生产部署应开启 Redis AOF/RDB，以持久化服务外部元数据。
