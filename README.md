# 归档平台：4+2 条带、降级读取、配额修复与对账

本仓库提供一个可运行的归档存储服务，覆盖：

- 对象按固定条带切分，每条带 4 个数据块 + 2 个校验块，落盘到 `block-0` 到 `block-5`。
- Redis 登记对象、条带、对象范围、块位置和块摘要；SQLite 保存审计、配额账本、修复任务与对账结论。
- 任意块目录下线后的降级读取；数据块缺失时先重建再返回，失败返回稳定错误码和条带/缺失块信息。
- 按条带修复到备站点目录，重建后重新做 RS 校验和逻辑条带 SHA-256 校验；未提交的重建块不参与读取。
- 每日写回配额、最多 2 个条带并发修复、风险优先调度、配额耗尽暂停。
- 主/备站点已写回块对账，输出缺失、长度不符、摘要不符三类差异。
- Prometheus 文本指标、稳定字段排序的 JSON 状态摘要、按条带审计查询。

## 实际运行环境

本次交付实际运行在：

- Windows 主机上的 WSL2 `Ubuntu 26.04`（`Linux ... microsoft-standard-WSL2`）。
- Go `1.22.12`。
- Redis `7.2.7`，从 `redis-7.2.7.tar.gz` 源码构建，`127.0.0.1:6379`。
- SQLite 使用 Go 驱动 `github.com/mattn/go-sqlite3 v1.14.24`（驱动内置 SQLite，无需额外服务）。
- 服务固定 `runtime.GOMAXPROCS(2)`；修复并发上限配置为 2。

`REPORT.md` 给出了这一轮的完整验收结果、峰值资源、重启续修、配额耗尽、风险优先和失败校验证据。

## 快速启动

```bash
# 1) 准备 Go 1.22+ 和 Redis 7.2（本环境已有）
export PATH="$PWD/.tools/go/bin:$PATH"
export GOMAXPROCS=2

# 2) 构建
go build -o bin/archive-server ./cmd/archive-server
go build -o bin/archive-check  ./cmd/archive-check

# 3) 启动 Redis 7.2.7（源码构建目录示例）
.tools/redis-7.2.7/src/redis-server \
  --bind 127.0.0.1 --port 6379 --daemonize yes \
  --pidfile "$PWD/var/redis/redis.pid" \
  --logfile "$PWD/var/redis/redis.log" \
  --dir "$PWD/var/redis" \
  --save '' --appendonly no --maxmemory 256mb --maxmemory-policy noeviction

# 4) 启动验收配置（127.0.0.1:18086，Redis DB 6，namespace archive:final）
./bin/archive-server -config config/acceptance-final.json

# 5) 生成固定种子的 200 个对象（约 300 MB）
curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/seed \
  -H 'Content-Type: application/json' \
  -d '{"profile":"full","seed_base":20261119,"skip_existing":true}'

# 6) 全量读取并校验固定种子内容
./bin/archive-check -addr http://127.0.0.1:18086 -mode healthy -ranges 2 \
  -json /tmp/check.json
```

主配置 `config/archive.json` 默认写回额度为 `1 GiB = 1073741824` 字节，可通过
`writeback_quota_bytes` 或 `ARCHIVE_WRITEBACK_QUOTA_BYTES` 覆盖。配额账本按
`Asia/Shanghai` 本地日期滚动。

## 配置

配置文件为 JSON，环境变量可覆盖。主要字段：

| 字段 | 默认值 | 说明 |
|---|---:|---|
| `listen_addr` | `127.0.0.1:18080` | HTTP 监听地址 |
| `data_root` | `./var/archive` | 块目录、备站点目录、临时目录根 |
| `redis_addr` | `127.0.0.1:6379` | Redis 地址 |
| `redis_db` / `redis_namespace` | `0` / `archive:v1` | 多实例隔离 |
| `sqlite_path` | `./var/archive/archive.db` | SQLite 文件 |
| `stripe_size` | `1048576` | 每个条带的逻辑字节数，必须能被 4 整除 |
| `data_blocks` / `parity_blocks` | `4` / `2` | 固定 4+2 |
| `writeback_quota_bytes` | `1073741824` | 每日备站点写回额度 |
| `repair_concurrency` | `2` | 修复条带并发上限，取值范围 1..2 |
| `auto_repair` | `true` | 是否启动后台调度器 |
| `repair_poll_ms` | `1000` | 后台调度轮询间隔 |
| `repair_delay_ms` | `0` | 调试/重启测试用延迟 |
| `timezone` | `Asia/Shanghai` | 配额日切时区 |
| `gomaxprocs` | `2` | 并行度上限 |

工作区布局由服务自动创建：

```text
<data_root>/
  blocks/block-0..5/<stripe-id>/<block-index>.blk
  standby/block-0..5/<stripe-id>/<block-index>.blk
  tmp/
<sqlite_path>
```

Redis 中每个条带保存 `offset_start/offset_end`、`length`、`shard_length`、
`data_sha256`、`blocks`（主/备路径、长度、SHA-256、是否提交）以及修复状态。

## HTTP API

### 对象写入与读取

```bash
# 服务端固定种子生成并写入
curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/objects \
  -H 'Content-Type: application/json' \
  -d '{"object_id":"demo","size":1048576,"seed":42}'

# 客户端原始字节上传
curl --noproxy '*' -X PUT http://127.0.0.1:18086/v1/objects/demo \
  -H 'Content-Type: application/octet-stream' --data-binary @demo.bin

# 指定范围读取（闭区间，毫秒级重建）
curl --noproxy '*' \
  'http://127.0.0.1:18086/v1/objects/demo?range=1000-2000'

# 标准 Range 头
curl --noproxy '*' -H 'Range: bytes=1000-2000' \
  'http://127.0.0.1:18086/v1/objects/demo'
```

读取响应头包含：

- `X-Archive-Degraded`：是否使用了备站点块或重建块。
- `X-Archive-Reconstructed-Blocks`：本次读取重建的块数。
- `X-Archive-Stripes-Touched`：本次读取涉及的条带数。

失败时返回 JSON：

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

稳定错误码包括 `OBJECT_NOT_FOUND`、`INVALID_RANGE`、
`STRIPE_UNRECOVERABLE`、`STRIPE_RECONSTRUCT_FAILED`、
`STRIPE_INTEGRITY_FAILED`、`STRIPE_METADATA_MISSING`、`LAYOUT_CHANGED`。
读取会先完成所有涉及条带的读取/重建/摘要校验，再写 HTTP 响应，不会返回拼接错误或截断内容。

### 块目录下线与恢复

```bash
curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/dirs/0/offline \
  -H 'Content-Type: application/json' \
  -d '{"reason":"simulated_bad_disk"}'

curl --noproxy '*' -X POST http://127.0.0.1:18086/v1/dirs/0/online \
  -H 'Content-Type: application/json' \
  -d '{"reason":"disk_replaced"}'
```

目录状态变更使用进程内读写锁与 Redis `layout_epoch`：

- 正在进行的读取持有读锁，目录切换持有写锁；读取要么看到旧布局，要么看到新布局。
- 读取结束前再次检查 Redis epoch；跨实例变更时最多重试 3 次，仍变化则返回 `LAYOUT_CHANGED`。

### 修复、队列与配额

```bash
# 查看风险优先队列、跳过原因和优先级解释
curl --noproxy '*' 'http://127.0.0.1:18086/v1/repairs/queue?limit=20'

# 手动触发一轮；wait=1 会等待可运行任务完成或配额暂停
curl --noproxy '*' -X POST \
  'http://127.0.0.1:18086/v1/repairs/drain?timeout_sec=240'

# 当日账本
curl --noproxy '*' http://127.0.0.1:18086/v1/quota
```

调度规则（可复核）：

1. `available_blocks` 升序，可用块越少越先，风险标签 `critical`/`high`。
2. 同等风险下，`last_read_at` 更早者优先；从未读取（空时间）最先。
3. 再按对象大小升序；最后按 `stripe_id` 升序。
4. 调度器只处理当前最高风险组。若该组有条带因额度不足未启动，立即停止，不跳到低风险组；
   因此低风险条带不会挤占高风险条带的额度。
5. 队列接口返回 `priority_reason`、`state`、`skip_reason`；被跳过的条带同时出现在 `skipped` 中。

修复语义：

- 每个条带使用 Redis 租约，跑满 4 个可用块后做 RS 重建。
- 重建后重新编码校验 6 个分片，并校验逻辑条带 `data_sha256`。
- 只有校验通过才写 `standby/...`，再提交 Redis `backup_state=committed`；
  提交前目标块不参与读取。
- 读取发现块长度/摘要不符时会把该块标记为不可用并唤醒修复队列，后续读取会走
  重建或已验证备块。
- SQLite `repair_jobs` 结果与 `quota_ledger` 状态在同一事务中提交；失败时释放预留额度。
- 失败后设置 30 秒冷却，避免坏数据导致紧循环重试；失败原因写入审计。
- 服务重启时 `resumeLoop` 加载 `state=running` 的任务继续执行，已提交任务不会再次扣配额。

### 对账

```bash
curl --noproxy '*' -X POST 'http://127.0.0.1:18086/v1/reconcile?wait=1'
curl --noproxy '*' http://127.0.0.1:18086/v1/reconcile/last
```

对账口径：

- 开始时快照 Redis 元数据和 6 个目录在线状态。
- 只比较快照中 `backup_state=committed` 的“已写回备站点块”；未提交/修复中块不报差异。
- 开始时下线的块目录整体排除，结论只反映下线目录之外的块集合。
- 两侧都存在时比较长度和 SHA-256；输出 `missing`、`length_mismatch`、`digest_mismatch` 三类数组。
- 对账文件读取不加全局读锁，允许并发写入、读取与修复；结论只使用开始快照。

### 审计、状态与指标

```bash
curl --noproxy '*' http://127.0.0.1:18086/v1/audit/stripes/seed-m-000-s000000
curl --noproxy '*' 'http://127.0.0.1:18086/v1/status?queue=10'
curl --noproxy '*' http://127.0.0.1:18086/metrics
```

状态摘要字段顺序固定，`block_dirs` 按 0..5、队列按风险优先级排序，不返回绝对路径。

Prometheus 指标：

```text
archive_block_dir_online{dir="block-0"} 1
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

## 固定种子数据

`POST /v1/seed` 的 `full` 轮廓固定生成：

- 140 个小对象，64 KiB..512 KiB；
- 60 个中等对象，1 MiB..4 MiB；
- 总计 305,677,698 字节（291.52 MiB），200 个对象。

对象内容由内置 SplitMix64 生成器按 `seed` 确定性产生，`archive-check` 用同一算法独立重算并校验。

## 构建与测试

```bash
export PATH="$PWD/.tools/go/bin:$PATH"
export GOMAXPROCS=2
go test ./...
go vet ./...
```

`archive-check` 支持 `healthy`、`degraded`、`unrecoverable` 三种校验模式：

```bash
./bin/archive-check -addr http://127.0.0.1:18086 -mode degraded -ranges 2 \
  -json artifacts/final/check-after-repair.json
```

## 仓库结构

```text
cmd/archive-server      HTTP 服务入口
cmd/archive-check       独立内容/范围校验器
internal/config         配置加载与校验
internal/codec          4+2 Reed-Solomon 编码/重建/校验
internal/model          对象、条带、目录、配额模型
internal/store          Redis 元数据与 SQLite 事务账本
internal/service        写入、读取、下线、修复、对账、指标
internal/httpapi        HTTP 路由与 Range 解析
internal/seed           固定种子数据生成器
config/                 主配置、验收配置、配额/风险/失败/重启测试配置
scripts/                资源采样脚本
artifacts/final/        最终验收产物
REPORT.md               设计与验收报告
```
