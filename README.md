# slotguard：复制槽保护与一致性订阅重引导

面向 Windows 11 单机双 PostgreSQL 实例的复制链保护服务：

- 主库 `127.0.0.1:55432`，备库 `127.0.0.1:55433`；
- 共享 Redis 7 `127.0.0.1:6379` 保存消费位点；
- 业务表默认是 `wms.outbound_order`（父表）和
  `wms.settlement_detail`（子表）；
- 服务只使用配置中的连接串，不启动或管理 PostgreSQL/Redis 进程。

服务解决四类问题：

1. 不活跃逻辑槽持续保留 WAL，超过阈值时给出分级、非破坏性的释压建议；
2. 订阅断流或槽被手工删除后，从主库建立同时覆盖父/子表的一致性起点并重引导；
3. 重同步进程或实例重启后可续跑，已提交批次不重写，重复同步不产生重复主键；
4. Redis 消费位点与备库实际应用位点核对，并持续做只读一致性抽样。

## 目录

- `src/config.py`：JSON + `SLOTGUARD_*` 环境变量配置。
- `src/db.py`：连接、LSN 工具、稳定错误码。
- `src/slots.py`：两实例逻辑槽/订阅统一视图与错误隔离。
- `src/relief.py`：两级释压建议，从不删除复制槽。
- `src/resync.py`：一致快照、原子批次水位、增量续跑、删除对齐、订阅重挂。
- `src/offsets.py`：Redis 位点核对和修正。
- `src/sampling.py`：固定主键哈希抽样，只读 SELECT。
- `src/store.py`：本地 SQLite 持久层及轮次事务。
- `src/metrics.py`：Prometheus 文本指标。
- `src/service.py` / `src/cli.py`：常驻循环与命令行。
- `scripts/`：建表种子、50 TPS 压测、故障注入、一键验收。
- `sql/schema.sql`：父/子表样本结构。

## 安装与启动

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt

$env:SLOTGUARD_PRIMARY_DSN = "host=127.0.0.1 port=55432 user=postgres dbname=postgres"
$env:SLOTGUARD_REPLICA_DSN = "host=127.0.0.1 port=55433 user=postgres dbname=postgres"
$env:SLOTGUARD_REDIS_URL    = "redis://127.0.0.1:6379/0"

python scripts/01_init_schema_seed.py
python src/cli.py resync --reason bootstrap
python src/cli.py run
```

Linux/macOS 开发环境同理，只需使用 `.venv/bin/python`。所有路径和命令均为
跨平台 Python；`scripts/env.sh` 仅提供可选的 PYTHONPATH 设置。

依赖要求：

- Python 3.11+；
- `psycopg[binary]>=3.2,<3.4`；
- `redis>=5,<9`；
- 主库账号需能读 `pg_replication_slots`、创建 publication/逻辑槽；
- 备库账号需能读/改 `pg_subscription`、写目标业务表；创建订阅通常要求超级用户。

前置数据库参数仍由使用方设置：`wal_level=logical`、足够的
`max_replication_slots`/`max_wal_senders`、复制用户与 `pg_hba`。

## 命令

| 命令 | 作用 |
| --- | --- |
| `python src/cli.py run` | 常驻监控、抽样、位点核对与指标 HTTP |
| `python src/cli.py slots` | 输出两实例统一槽视图 JSON |
| `python src/cli.py monitor` | 单次槽汇总与释压判定 |
| `python src/cli.py advices` | 待处理释压建议 |
| `python src/cli.py resync` | 启动或继续一致性重引导 |
| `python src/cli.py resync-progress` | 查询每张表阶段、主键水位和已处理行数 |
| `python src/cli.py reconcile` | 核对并修正 Redis 消费位点 |
| `python src/cli.py sample` | 执行一轮只读一致性抽样 |
| `python src/cli.py verify` | 校验主备行数、孤儿明细和已应用 LSN |
| `python src/cli.py metrics` | 打印一次 Prometheus 文本 |

`--config` 可放在子命令前：`python src/cli.py --config config.json monitor`。
配置字段均可用同名大写环境变量覆盖，例如
`SLOTGUARD_WAL_RETENTION_THRESHOLD_BYTES=268435456`。

## 关键实现

### 1. 槽视图与错误隔离

每个实例分别查询 `pg_replication_slots`、`pg_subscription` 和
`pg_replication_origin_status`，按槽名合并。视图中包含：

- 实例、槽名、插件、槽类型、活跃状态、PID；
- `restart_lsn`、`confirmed_flush_lsn`、实例当前 WAL；
- `lag_bytes`、`retained_wal_bytes`、`inactive_since/seconds`；
- 同槽是否同时出现在主备两侧（`exists_on_both`）。

连接失败固定为 `SG-E1001`，查询失败固定为 `SG-E1002`，错误内带实例和查询
对象。槽查询失败不会阻止同一实例的订阅查询，也不会阻止另一实例汇总。

### 2. 分级释压，不删槽

仅对不活跃且保留 WAL 超过 `wal_retention_threshold_bytes` 的逻辑槽给出建议：

1. 首次超阈值：`notify_catchup`，通知对应订阅方补拉；
2. 连续 `relief_recheck_rounds` 轮无进展：`recycle_candidate`，同时给
   `deferred_reclaim=true`，保留槽名、实例、订阅方标识、restart/confirmed LSN；
3. 槽恢复活跃、积压回落、或槽不存在：同一 SQLite 事务内把待处理建议改为
   `revoked`。

服务没有任何 `pg_drop_replication_slot` 调用。`relief_min_inactive_s` 可额外
要求槽至少不活跃若秒数（默认 0，便于短故障测试）。

### 3. 一致性重同步起点

重同步不直接把“当前 WAL 位置”当作快照起点。实际顺序是：

1. 锁连接对父/子表取得 `SHARE` 锁，等待已有写事务结束并阻止新写；
2. 另一个新事务在该锁窗口内创建 `pgoutput` 逻辑槽；
3. 导出事务快照，并由独立连接 `SET TRANSACTION SNAPSHOT` 导入；
4. 在锁窗口内写一条非事务逻辑消息，作为快照之后的 WAL 边界；
5. 导入连接保持整段拷贝，锁连接释放后业务写入继续进入逻辑槽。

因此快照看见的数据严格位于同一状态，槽从快照之后的边界继续。拷贝完成后使用
`pg_replication_slot_advance` 推进到该边界，再把订阅 origin 重置到边界并启用，
不会重放已由快照覆盖的行。

常驻模式默认开启 `auto_resume_resync` 和 `auto_rebootstrap`：启动时续跑未完成
任务；监控到配置槽缺失且两实例均可读时，自动执行同一重引导流程。正常“订阅被
禁用但槽仍存在”只进入释压建议，不会误触发全量重同步。

### 4. 可续跑与不重做

每批数据在备库执行 UPSERT，并在**同一个备库事务**中更新
`slotguard.resync_progress` 水位；SQLite 只是可查询镜像。重启时取备库水位和
SQLite 水位的最大值：

- 已提交批次不会重做；
- 中断后的新快照通过 `updated_at` 增量补写旧键区间；
- 最后用主键临时表做 anti-join，删除同步期间被主库删除的目标行；
- 先父表、后子表拷贝，恢复/校验外键后再进入订阅追平；
- UPSERT + 主键约束保证重复同步不产生重复主键。

目标表没有时间版本列时仍可续跑，但恢复后会从主键起点做幂等全表重扫，避免漏掉
两次快照之间对已拷贝行的更新；配置样本中的 `updated_at` 可走增量路径。该实现
面向整数主键（样本结构使用 bigserial）。

### 5. 订阅重挂

服务先禁用已有订阅，清空/核对目标数据，推进槽到快照边界，然后：

- 不存在订阅时以 `copy_data=false, create_slot=false` 创建；
- 已存在时更新 `slot_name` 和 publication；
- 将 `pg_<subid>` replication origin 精确设置到边界；
- 启用订阅，等待 worker 的 received/latest-end LSN 追平并校验行数与孤儿。

不删除旧槽是默认行为；配置的 `slot_name` 不存在时会重建。

### 6. Redis 位点

权威位点是备库订阅 origin 的 `remote_lsn`（实际 apply 位置）。消费位点键为：

```text
<redis_key_prefix>offset:<slot_name>
```

支持纯 LSN 文本或 JSON `{"lsn":"..."}`。判定：

| 判定 | 处理 |
| --- | --- |
| `missing` | 启用备库 apply LSN |
| `behind` | 修正为备库 apply LSN |
| `ahead_rejected` | 绝不采用超前值，回退到备库 apply LSN |
| `invalid_rejected` | 非法 LSN 不采用 |
| `equal` | 不改写 |

修正值直接写回同一个 key，后续启动复用。服务只读写该位点 key，不删除 Redis
中的其他键。

### 7. 只读抽样

按配置种子对父表主键做 `hashtextextended` 排序，固定抽取
`sample_order_count` 条出库单，再查询其全部结算明细。比对：

- 出库单：`status`、`total_amount`；
- 结算明细：`status`、`amount`、`unit_price`、`quantity`。

不一致项写入 `sample_mismatches`，包括轮次、表名、主键、字段和两侧值。抽样只
执行 SELECT，不写任一实例的业务表。

### 8. SQLite 轮次事务

`rounds` 先提交一个 `running` 标记，用于崩溃恢复；本轮槽快照、查询错误、释压
建议、位点修正或抽样不一致与 `rounds.status='done'` 在同一个结论事务中提交。
崩溃只会留下 running 标记，启动时按相同 `round_id` 继续。已完成轮次对同一
`round_id` 的写入会被拒绝，不会被后续轮次覆盖。

## Prometheus 指标

默认 `http://127.0.0.1:9721/metrics`：

| 指标 | 标签 |
| --- | --- |
| `slotguard_replication_slots` | `instance` |
| `slotguard_inactive_replication_slots` | `instance` |
| `slotguard_slot_lag_bytes_max` | `instance` |
| `slotguard_retained_wal_bytes_max` | `instance` |
| `slotguard_resync_tables_in_progress` | - |
| `slotguard_offset_corrections_total` | - |
| `slotguard_sample_mismatches_total` | - |
| `slotguard_query_errors_total` | - |

`instance` 固定为 `primary` / `replica`。`monitor_interval_s` 默认 10 秒。

## 故障注入与验收

```bash
python scripts/03_fault_inject.py disconnect
python scripts/03_fault_inject.py drop-slot
python scripts/03_fault_inject.py gap
python scripts/03_fault_inject.py tamper --ahead
python scripts/03_fault_inject.py tamper --behind
python scripts/03_fault_inject.py delete-offset

python scripts/02_workload.py --duration 60 --rate 50
python scripts/04_acceptance.py
```

`04_acceptance.py` 会自动补建 schema/订阅，并验证 21 项：错误隔离、主备同槽
标记、两级释压、自动撤销、缺失/超前/落后位点、只读抽样、并发 50 TPS 重同步、
强杀续跑、重复同步、未完成轮次恢复、指标和刷新时延。

隔离环境实测结果：

- 21/21 验收通过；
- 重同步期间新增写入能够追平，最终行数一致、孤儿为 0；
- 6 万出库单 + 12 万明细首次重引导约数秒；
- 50 TPS 压测 P95 约 2.7-3.0ms（要求 ≤50ms）；
- 单次监控刷新约 0.01-0.2s（要求 ≤15s）；
- 强杀后续跑从备库已提交水位继续，不重复主键。
- 常驻服务运行中手工删槽后，约 5s 内完成自动重引导并恢复订阅。

## 配置

见 `config.json`。常用字段：

| 字段 | 说明 |
| --- | --- |
| `primary_dsn`, `replica_dsn` | 两实例连接串 |
| `redis_url`, `redis_key_prefix` | Redis 与键前缀 |
| `publication_name`, `subscription_name`, `slot_name` | 复制对象名 |
| `parent_table`, `child_table` | 父/子表 |
| `parent_key`, `child_key`, `child_fk` | 主键与外键列 |
| `*_version_column` | 续跑增量列，默认 `updated_at` |
| `child_constraint_name` | 重同步后恢复的外键名 |
| `wal_retention_threshold_bytes` | 积压阈值，默认 256 MiB |
| `relief_recheck_rounds` | 无进展升级轮数 |
| `resync_batch_size` | 每批行数 |
| `resync_wait_timeout_s` | 订阅追平超时 |
| `auto_resume_resync` | 服务启动时自动续跑未完成重同步 |
| `auto_rebootstrap` | 配置槽缺失时自动启动重同步 |
| `resync_retry_interval_s` | 自动重同步失败重试冷却 |
| `sampling_interval_s`, `reconcile_interval_s` | 周期任务间隔 |
| `monitor_interval_s` | 槽状态刷新间隔，应 ≤15 |
| `sqlite_path` | 本地结论库 |

## 稳定错误码

- `SG-E1001`：实例连接失败；
- `SG-E1002`：状态查询失败；
- `SG-E2001`：订阅不存在；
- `SG-E2002`：订阅尚无 replication origin 状态；
- `SG-E2003`：无效 apply LSN；
- `SG-E3001`：槽插件不匹配；
- `SG-E3002`：配置键列不存在；
- `SG-E3003`：重同步中槽消失；
- `SG-E3004`：重同步中槽仍活跃。

服务不自动删槽。生产上若决定延后回收，应由具备权限的运维在确认订阅方标识、
确认位点和业务影响后执行，并保留本服务 SQLite 中的审计记录。
