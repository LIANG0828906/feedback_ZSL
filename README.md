# 本地多版本对象桶服务

这是一个只依赖 Python 标准库（负载脚本额外使用 `requests`）的本地对象桶服务，面向文件归档备份。服务在同一主机上维护两个相互独立、均带持久化提交日志的存储域：

- `main`：主桶，归档客户端写入、删除、建快照和回滚。
- `backup`：备桶，通过 HTTP 从主桶的提交序号增量追赶。

每个桶拥有自己的 SQLite 元数据文件和内容寻址对象目录，不共享文件或数据库。服务绑定 `127.0.0.1`，不需要外部网络或真实对象存储。

## 快速开始

要求：

- Windows 10/11、Windows Server 2022 或同等工作环境
- Python 3.11+
- Python 标准库 `sqlite3`
- `requests`（实际流转只用于负载/测试客户端，服务本身使用标准库）

安装校验：

```text
python -c "import sqlite3, requests; print(sqlite3.sqlite_version, requests.__version__)"
```

启动服务（`--port 0` 会自动选择空闲端口）：

```text
python -m archive_bucket serve --root data --host 127.0.0.1 --port 0
```

启动日志会输出一行机器可读的地址：

```json
READY {"base_url": "http://127.0.0.1:54321", "root": "data"}
```

也可以固定端口，例如 `--port 8765`。若同机并行运行多个实例，请为每个实例指定独立 `--root` 和端口，或让端口保持 `0`。

数据目录布局：

```text
data/
  main/
    bucket.sqlite3
    blobs/<hash-prefix>/<sha256>.bin
    tmp/
  backup/
    bucket.sqlite3
    blobs/<hash-prefix>/<sha256>.bin
    tmp/
```

对象数据不会写入状态或指标响应，状态中也不包含绝对路径。

## 数据模型与保证

- 每次 `PUT` 创建新修订，不覆盖旧修订。
- `revision_id` 在每个桶内由独立计数器单调递增；同步时备桶保留主桶的修订标识。
- 每次写入/删除/回滚都会生成一个桶内单调递增的 `commit_seq`。
- `DELETE` 只把键设为不可见，不删除修订元数据或内容。
- 快照保存快照提交序号和每个可见键对应的修订集合。
- 创建快照与写入共享同一个写锁，因此并发写入下快照不会混合新旧集合。
- 回滚会产生一个新的 `restore` 提交。回滚后可见集合与目标快照逐键一致，快照之后产生的修订仍作为历史保留。
- 多键一致性读取使用 `GET /v1/buckets/{bucket}/view`，该响应在一个读锁/事务边界内生成。单键读取也遵循同一锁边界。
- 回收只选择“当前不可见且不被任何现存快照引用”的修订。主桶还会暂缓回收备份尚未应用到的提交，以免破坏中断后的增量追赶。
- 按修订读取从查询到返回内容都持有读锁；回收持有写锁，因此不会读取到“元数据存在但文件刚刚被删”的中间态。已回收修订返回 `410 revision_reclaimed`。

## HTTP API

所有 JSON 响应都由同一稳定序列化器输出：字段名排序、分隔符固定。错误响应示例：

```json
{
  "code": "not_found",
  "details": {"key": "archive/absent.bin"},
  "error": "not_found",
  "message": "visible revision not found",
  "status": 404
}
```

对象键在路径中应做 URL 编码。例如 `archive/demo.bin` 编码为 `archive%2Fdemo.bin`。

### 健康、状态和指标

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/health` | 存活检查 |
| GET | `/v1/status` | 两桶状态、同步进度、最近核对结论、配置摘要 |
| GET | `/metrics` | Prometheus 文本格式 |

状态中的关键路径：

- `buckets.main` / `buckets.backup`
- `sync.main_latest_commit_seq`
- `sync.applied_commit_seq`
- `sync.lag_commits`
- `sync.caught_up`
- `comparison`

Prometheus 指标包括两桶的：

- `object_bucket_visible_objects`
- `object_bucket_occupancy_bytes`
- `object_bucket_latest_commit_seq`
- `object_bucket_synced_commit_seq`
- `object_bucket_snapshots`
- `object_bucket_reclaimed_bytes`
- `object_bucket_reclaimed_revisions`
- `object_bucket_snapshot_referenced_revisions`
- `object_bucket_consistency_mismatch_keys`
- `object_bucket_sync_lag_commits`

### 对象与修订

| 方法 | 路径 | 说明 |
|---|---|---|
| PUT | `/v1/buckets/{bucket}/objects/{key}` | 写入新修订，返回 `commit_seq`、`revision_id`、`content_hash`、`size` |
| GET | `/v1/buckets/{bucket}/objects/{key}` | 读取最新可见修订 |
| GET | `/v1/buckets/{bucket}/objects/{key}/revisions/{revision_id}` | 按修订读取 |
| GET | `/v1/buckets/{bucket}/objects/{key}/revisions` | 列出一个键的全部历史修订和可见性 |
| DELETE | `/v1/buckets/{bucket}/objects/{key}` | 删除可见性，返回新 `commit_seq` |
| GET | `/v1/buckets/{bucket}/objects?prefix=&limit=` | 按键列出可见对象 |
| GET | `/v1/buckets/{bucket}/view?commit_seq=` | 获取当前或指定提交序号的原子多键视图 |

二进制响应头：

- `ETag`
- `X-Content-SHA256`
- `X-Object-Revision-Id`
- `X-Bucket-Commit-Seq`

写入示例：

```text
curl -X PUT --data-binary @backup.tar \
  http://127.0.0.1:54321/v1/buckets/main/objects/archive%2Fbackup.tar
```

可带期望哈希：

```text
curl -X PUT -H "X-Content-SHA256: <sha256>" --data-binary @backup.tar \
  http://127.0.0.1:54321/v1/buckets/main/objects/archive%2Fbackup.tar
```

读取指定修订：

```text
curl http://127.0.0.1:54321/v1/buckets/main/objects/archive%2Fbackup.tar/revisions/7
```

### 快照与回滚

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/buckets/{bucket}/snapshots` | 创建快照；可选 JSON 字段 `snapshot_id` |
| GET | `/v1/buckets/{bucket}/snapshots` | 列出快照 |
| GET | `/v1/buckets/{bucket}/snapshots/{snapshot_id}` | 快照详情和覆盖集合 |
| DELETE | `/v1/buckets/{bucket}/snapshots/{snapshot_id}` | 删除快照并释放其修订引用 |
| POST | `/v1/buckets/main/snapshots/{snapshot_id}/restore` | 回滚主桶 |
| POST | `/v1/buckets/main/rollback` | 回滚别名，JSON 为 `{"snapshot_id":"..."}` |

创建快照响应包含：

- `snapshot_id`
- `commit_seq`
- `object_count`
- `revision_count`
- `visible`：`key -> revision_id`
- `entries`：逐项包含键、修订、哈希、大小

### 增量同步

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/sync` | 从主桶 HTTP 提交日志追赶备桶 |
| POST | `/v1/buckets/backup/sync` | 同上 |
| GET | `/v1/buckets/main/commits?after_seq=&limit=` | 查看提交事件；同步客户端也使用此接口 |

请求体可选：

```json
{
  "source": "http://127.0.0.1:54321",
  "limit": 100,
  "max_events": 10000
}
```

省略 `source` 时使用当前服务自身地址。同步结果包含：

- `from_seq`
- `applied_since_start`
- `applied_seq`
- `latest_source_seq`
- `lag_commits`
- `paused`

每个提交的事件应用、备桶元数据写入和 `applied_seq` 推进在同一事务中完成。每个 `PUT` 内容先按哈希落盘，内容校验通过后才提交元数据。中断后重新同步只会读取 `applied_seq` 之后的事件。

### 回收与占用

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/buckets/{bucket}/stats` | 返回对象数、占用字节、快照引用数、回收计数等 |
| POST | `/v1/buckets/{bucket}/gc` | 按引用关系回收；可选 `{"target_bytes": N}` |
| POST | `/v1/gc` | 回收两桶 |

`stats` 示例字段：

```json
{
  "applied_seq": 0,
  "latest_commit_seq": 42,
  "occupancy_bytes": 125829120,
  "reclaimed_bytes": 7340032,
  "reclaimed_revision_count": 4,
  "revision_count": 210,
  "snapshot_count": 3,
  "snapshot_referenced_revisions": 24,
  "visible_object_count": 200
}
```

默认上限为 `200 MiB`。达到 95% 水位时，写入路径会尝试自动回收；主桶与备桶都可能触发。也可以通过故障注入接口降低水位，以在测试负载中触发自动回收。

### 跨桶一致性核对

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/consistency/check` | 按快照或提交序号核对 |
| GET | `/v1/consistency/check?snapshot_id=` | 查询参数形式 |
| GET | `/v1/consistency` | 最近一次核对结果 |

请求示例：

```json
{
  "snapshot_id": "snap_20261119T010203_abcdef",
  "verify_content": true
}
```

或：

```json
{
  "commit_seq": 128,
  "verify_content": true
}
```

核对开始时会捕获备桶 `applied_seq`，最终核对序号为：

```text
checked_commit_seq = min(请求或快照序号, 核对开始时备桶已应用序号)
```

同步可以在核对进行时继续推进，但不会影响本次结果覆盖的序号。核对逐键比较可见修订标识以及实际内容 SHA-256。差异原因固定为：

- `missing`：一侧缺失键或修订内容
- `revision_behind`：两侧可见修订标识不同；`direction=backup_ahead` 表示备桶实际状态超前
- `content_different`：修订标识相同但内容哈希不同

每条差异都包含 `key`、两侧 `revision_id`、两侧 `content_hash` 和 `reason`。

### 故障注入

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/faults` | 查看当前注入状态 |
| POST | `/v1/faults` | 更新注入状态 |

可设置字段：

```json
{
  "sync_pause": false,
  "sync_pause_after": 7,
  "backup_write_failure_once": true,
  "occupancy_limit_bytes": 167772160,
  "gc_high_watermark_bytes": 136314880,
  "gc_target_bytes": 125829120,
  "auto_gc": true
}
```

- `sync_pause_after`：本次同步成功应用 N 个事件后暂停，并把 `sync_pause` 置为 `true`。
- `backup_write_failure_once`：下一次同步在写入备桶前失败一次，返回 `503`，已应用序号不前进。
- `occupancy_limit_bytes`：更新默认 200 MiB 上限，同时推导 95% 高水位和 90% 目标。
- `gc_high_watermark_bytes` / `gc_target_bytes`：可覆盖推导出的水位。

### 内置负载生成

直接在服务内构造确定性数据集：

```text
curl -X POST -H "Content-Type: application/json" \
  -d "{\"seed\":20261119,\"count\":200,\"target_bytes\":125829120,\"prefix\":\"archive/demo\",\"revisions\":1,\"rate_per_second\":0}" \
  http://127.0.0.1:54321/v1/load/generate
```

生成器固定按：

- 约 190 个 4 KiB–28 KiB 小对象
- 8 个中等对象
- 2 个几十 MiB 大对象

构造约 120 MiB 数据。内容由 `SHAKE-256(seed|key|revision|size)` 确定性生成，不依赖外部数据。

## 一键验证场景

完整场景会启动服务、构造 200 个对象、双客户端并发写入、并发快照、删除、可回收历史、同步暂停/写入失败恢复、回滚并发读取、引用回收、最终一致性核对和指标检查。

```text
python -m archive_bucket demo \
  --root data-demo \
  --count 200 \
  --target-mib 120 \
  --history-count 12 \
  --output artifacts/scenario_result.json
```

可配置速率（每秒操作数，`0` 表示不限速）：

```text
python -m archive_bucket demo \
  --write-rate 40 \
  --delete-rate 10 \
  --snapshot-rate 1 \
  --output artifacts/scenario_result.json
```

也可对已启动服务单独运行场景：

```text
python -m archive_bucket.scenario \
  --base-url http://127.0.0.1:54321 \
  --prefix archive/run-001 \
  --output artifacts/scenario_result.json
```

结果记录包含：

- `write_count`、`delete_count`、`snapshot_count`
- `sync_lag_commits`、`max_observed_sync_lag_commits`、`sync_runs`
- `faults` 与同步恢复序号
- `rollback_count`、`rollback_result`
- `reclaimed_bytes`、`reclaimed_revisions`（两桶累计）及 `gc` 中的显式回收明细
- `snapshot_comparison_consistent`
- `consistency`（最终逐键核对结论）
- `final_status`、`duration_seconds`、`rates_per_second`

场景失败时也会写入包含 `status: "failed"` 和错误摘要的结果文件。

## 测试

```text
python -m unittest discover -s tests -v
```

测试覆盖：

- 多版本写入、按修订读取、删除可见性
- 快照创建、快照并发写入一致性、回滚
- 增量同步暂停、备份写入失败、续传、服务重启
- 引用感知回收、快照保留、已回收修订 `410`
- 缺失、修订落后、内容不同三类核对结果
- 状态字段稳定排序、无绝对路径、Prometheus 指标

## 资源说明

- 服务无需外部网络。
- 默认单对象上限 64 MiB。
- 120 MiB 数据集在主备桶各存一份，另加历史修订和场景临时历史；完整演示峰值临时数据明显低于 2 GB。
- HTTP 响应和请求体采用有界读取；核对按 1 MiB 分块计算 SHA-256。
- 并行环境中建议每个实例独立数据目录，并用端口 `0` 或互不冲突的端口隔离。
