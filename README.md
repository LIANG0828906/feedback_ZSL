# 本地三域合规归档服务

本项目实现一个无外部依赖的本地合规归档服务，面向 Windows 10/11、Windows Server 2022 和 Python 3.11+。元数据使用 SQLite WAL，对象正文分别保存在 `primary`、`replica-a`、`replica-b` 三个独立存储域。

服务重点解决以下故障模式：

- 保留策略从 180 天延长到 365 天后，旧清理逻辑仍按 180 天删除 7 个月前的审计对象。
- 对象元数据与独立锁表在并发发布窗口内出现 `365` 与 `180` 的截止时间冲突。
- 清理在部分域成功后中断，重跑时不能注销仍存在副本的锁，也不能重复处理已完成版本。
- 对账期间允许写入和删除，但删除中的版本不应被误报为跨域差异。

## 快速启动

Windows PowerShell：

```powershell
py -3.11 -m archive_service --data-dir .\archive-data --host 127.0.0.1 --port 8080
```

或使用仓库内脚本：

```powershell
.\run.ps1 -DataDir .\archive-data -Port 8080
```

Linux/macOS：

```bash
python3 -m archive_service --data-dir ./archive-data --host 127.0.0.1 --port 8080
```

服务不访问网络，也不依赖第三方 Python 包。SQLite 由 Python 3.11 自带的 `sqlite3` 模块提供（要求 3.40+）。

数据目录布局：

```text
archive-data/
  archive.db
  domains/
    primary/<shard>/<version_id>.bin
    replica-a/<shard>/<version_id>.bin
    replica-b/<shard>/<version_id>.bin
  tmp/
```

对象正文不会与元数据写在同一个文件；版本文件路径由版本 ID 决定，因此删除同一对象键的某个版本不会影响其他版本。

## 保留策略语义

三类保留期为 `regular`、`audit`、`legal`，API 也接受 `legal_hold_days` 作为 `legal_days` 的别名。

- 发布时允许指定 `effective_at`。新写入版本按写入时间对应的已生效配置计算截止时间。
- 已存在版本只会被延长，不会因配置发布而缩短。发布生效时，元数据和未固定的锁记录都会以 `max(旧截止时间, 新计算值)` 更新。
- 三类中任一新值小于发布时对应类别的已生效值，整次发布原子拒绝，原配置不变：

```json
{
  "error": {
    "code": "RETENTION_POLICY_SHORTENING_REJECTED",
    "details": {
      "publication_id": 2,
      "effective_at": "2026-11-19T01:00:00.000000Z",
      "shortened_categories": {
        "audit": {"current": 365, "proposed": 180}
      }
    },
    "message": "a retention class may not be shortened"
  }
}
```

- 写入时可设置 `legal_hold=true`。法定封存标记不因截止时间到期而解除，删除准入始终拒绝。
- 对 `legal_hold=true` 的版本，计算截止时间时至少使用 `legal` 类保留期。

## HTTP API

所有 JSON 响应使用稳定键排序。错误统一为：

```json
{"error":{"code":"STABLE_CODE","details":{},"message":"..."}}
```

### 策略

`GET /api/v1/policies`

返回当前生效策略、最新发布和最近 50 次发布历史。

`POST /api/v1/policies`（也支持 `PUT`）

```json
{
  "regular_days": 365,
  "audit_days": 365,
  "legal_days": 365,
  "effective_at": "2026-11-19T10:00:00+08:00"
}
```

不传 `effective_at` 时立即生效。成功返回 `201`。

`POST /api/v1/policy-impact`

预览一次尚未发布的策略影响：

```json
{
  "regular_days": 365,
  "audit_days": 365,
  "legal_days": 365,
  "as_of": "2026-11-19T10:00:00+08:00"
}
```

查询一次已经完成的发布：

```json
{"publication_id": 2, "as_of": "2026-11-19T10:00:00+08:00"}
```

报告包含：

- `affected_version_count`、`affected_object_count` 和 `affected_versions`：受策略类别变化或截止时间变化影响的版本及对象数。
- `retention_deadline_changed_count`：截止时间被实际延长的版本数。
- `would_shorten_rejected_count`：策略预览会被缩短保护拒绝的版本数。
- `shortening_prevented_count`：已发布策略对已有更长截止时间的保护次数。
- `cleanup_admission_changes`：清理准入结果发生变化的版本清单。
- `report_id`：由发布 ID/配置和版本集合确定；同一配置与同一对象集合重复生成时稳定。只有 `generated_at`、`as_of` 等时间字段可变。

### 写入、读取、准入与删除

`PUT /api/v1/objects?key=<urlencoded-key>&category=audit&legal_hold=false`

请求体是原始字节，必须带 `Content-Length` 和 `Content-Type: application/octet-stream`。这是 20MiB 对象和压测使用的路径。

也可用 JSON：

```json
{
  "object_key": "records/audit/report-001.bin",
  "category": "audit",
  "legal_hold": false,
  "content_base64": "cmVwb3J0..."
}
```

成功写入返回 `201`，任一域失败返回 `502`，但仍返回可审计的版本信息和缺失域：

```json
{
  "version_id": "8a1f...",
  "version_number": 2,
  "status": "partial",
  "complete": false,
  "missing_domains": ["replica-b"],
  "retention_until": "2027-11-19T02:00:00.000000Z",
  "earliest_deletable_at": "2027-11-19T02:00:00.000000Z",
  "content_length": 16400,
  "content_sha256": "...",
  "domains": [
    {"domain": "primary", "status": "written", "length": 16400, "sha256": "..."},
    {"domain": "replica-a", "status": "written", "length": 16400, "sha256": "..."},
    {"domain": "replica-b", "status": "missing", "length": null, "sha256": null}
  ]
}
```

同一 `object_key` 的后续写入创建新版本，不覆盖旧版本。

普通版本的 `earliest_deletable_at` 等于保留截止时间；`legal_hold=true` 时该字段为 `null`（封存解除前没有可删除时间），但 `retention_until` 仍返回。

读取接口：

- `GET /api/v1/versions/<version_id>`：按版本读取正文，必要时从任一仍存在的域故障转移。
- `GET /api/v1/versions/<version_id>/metadata`：版本、三域摘要、元数据截止时间和锁记录。
- `GET /api/v1/versions/<version_id>/admission`：只判定、不删除。
- `GET /api/v1/objects/read?key=<key>&version=<version_id-or-number>`：按键和版本读取。
- `GET /api/v1/objects/<key>/versions/<version>/metadata`：按键和版本查询。

`DELETE /api/v1/versions/<version_id>`

先执行删除准入。允许时逐域删除；任一域失败则停止后续删除并保留尚未删除的副本，返回 `502` 和失败域；完全删除返回 `200`。准入拒绝时返回 `409`，`details.admission` 包含：

- `version_id`、`object_key`、`version_number`。
- `retention_resolution.metadata_retention_until` 与 `lock_retention_until`。
- `retention_resolution.effective_retention_until`，以及 `"policy":"maximum"`。
- `conflict.fields`、`reason_code`、`reason_codes`、`inconsistency_reason_code`、`decision_basis`。

法定封存的 `reason_code` 为 `LEGAL_HOLD_ACTIVE`；未来截止时间冲突为 `RETENTION_CONFLICT_ACTIVE`，且同时包含 `RETENTION_DEADLINE_MISMATCH`。拒绝发生在任何域文件删除之前。

### 清理

`POST /api/v1/cleanup`

```json
{"dry_run": false, "fail_domains": []}
```

清理以版本为单位，只处理元数据与锁记录取较晚值后已到期、且未法定封存的版本。每个域处理结果写入 `domain_copies.delete_status`：

1. 版本标记为 `cleaning`。
2. 按 `primary`、`replica-a`、`replica-b` 顺序删除。
3. 某域失败时标记 `failed`，停止后续域，版本变为 `cleanup_partial`，锁记录保留。
4. 所有实际存在的域都变为 `deleted` 后，才将版本标记 `deleted` 并注销锁记录。
5. 重跑只选择 `complete`、`partial`、`cleanup_partial` 版本，已 `deleted` 的域不会重复删除。

服务重启时，遗留的 `cleaning` 会自动恢复为 `cleanup_partial`，锁不会被误注销。

### 跨域对账

`POST /api/v1/reconcile`

对账开始时在数据库事务中快照所有 `complete` 且三域均存在的版本。随后逐域流式计算 SHA-256，不把完整对象加载到内存。写入的新版本不在快照中；开始后被删除或进入清理的版本会在输出前剔除。

输出示例：

```json
{
  "reconciliation_id": "...",
  "snapshot_version_count": 180,
  "checked_domain_copies": 540,
  "difference_count": 2,
  "missing_domains": [
    {"version_id":"...","object_key":"...","domain":"replica-a","kind":"missing"}
  ],
  "length_mismatches": [],
  "digest_mismatches": [
    {"version_id":"...","object_key":"...","domain":"replica-b","kind":"digest_mismatch"}
  ],
  "differences": []
}
```

`differences` 包含三类差异的完整列表；`missing_domains`、`length_mismatches`、`digest_mismatches` 是同一结论的分类视图。

### 状态与指标

`GET /api/v1/status`

返回最近一次清理和最近一次对账的完整摘要、当前策略、版本数和待清理版本数。JSON 键稳定排序，不包含任何绝对路径。

`GET /metrics`

Prometheus 文本格式，指标名和标签稳定：

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `archive_domain_object_versions{domain="primary"}` | gauge | 各域当前仍存在的版本副本数 |
| `archive_domain_bytes{domain="primary"}` | gauge | 各域当前逻辑占用字节 |
| `archive_pending_cleanup_versions` | gauge | 已到期且未封存、待清理版本数 |
| `archive_retention_conflict_delete_denials_total` | counter | 因截止时间冲突且较晚值仍在未来而拒绝删除的次数 |
| `archive_reconciliation_differences` | gauge | 最近一次对账的差异条目数 |

三个域标签固定为 `primary`、`replica-a`、`replica-b`。

## 固定种子样本

以下命令向新服务写入 150 个对象键、180 个版本、约 150MiB 原始内容（三域合计约 450MiB）。前 120 个键是 4KiB–200KiB 小对象，后 30 个键是 1MiB–5MiB 审计报告；部分键有 2 个版本。

先启动空数据目录的服务，再执行：

```bash
python3 -m scripts.generate_dataset \
  --base-url http://127.0.0.1:8080 \
  --seed 283 \
  --extend-policy \
  --policy-days 365 \
  --conflicts 40 \
  --output artifacts/incident-sample.json
```

生成器行为：

1. 所有版本按默认 180 天策略写入，`written_at` 约为 210 天前。
2. 发布三类 365 天策略，已有版本截止时间被延长。
3. 从审计版本中选 40 个，将元数据截止时间固定为 365 天、锁记录固定为 180 天，模拟发布窗口并发写入造成的锁表滞后。

Windows 使用 `py -3.11 -m scripts.generate_dataset ...`。`--dry-run` 可先查看键数、版本数和字节数。固定 `--seed` 决定键序、版本数和正文内容。

## 本地验证

运行全部集成测试：

```bash
python3 -m unittest discover -s tests -v
```

测试覆盖：

- 180→365 延长、缩短发布 `409` 拒绝且配置保持。
- 元数据 365/锁 180 的冲突准入、删除拒绝、三域文件均未删除、拒绝计数器。
- 法定封存即使到期也拒绝。
- 单域写入故障返回缺失域且不标记三域完整。
- 清理单域失败后保留剩余副本、锁不注销、重跑续传，且读取其他版本不受影响。
- 对账输出缺失域、长度不符、摘要不符，并排除已删除版本。
- 影响报告确定性及准入变化清单。
- JSON 状态不含绝对路径，Prometheus 指标存在。
- HTTP 原始字节写入、读取、清理和删除后 `410`。

复现冲突样本的准入拒绝：

```bash
VID=$(python3 -c "import json;print(json.load(open('artifacts/incident-sample.json'))['conflicts'][0]['version_id'])")
curl -i -X DELETE "http://127.0.0.1:8080/api/v1/versions/$VID"
curl -s http://127.0.0.1:8080/metrics | grep retention_conflict
```

预期 `DELETE` 返回 `409 RETENTION_CONFLICT_ACTIVE`，域文件数不变，冲突拒绝计数加一：

```text
archive_retention_conflict_delete_denials_total 1
```

全量对账：

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/reconcile
curl -s http://127.0.0.1:8080/api/v1/status
```

## 性能验证

脚本在独立子进程中启动服务，通过 HTTP 写入和读取 20MiB 对象、写入完整 150MiB 数据集并并发执行全量对账与清理，同时采样服务进程峰值 RSS：

```bash
python3 -m scripts.benchmark \
  --full-dataset \
  --write-size-mib 20 \
  --write-samples 6 \
  --read-size-mib 20 \
  --read-samples 30
```

当前工作区实测（WSL/Linux；Python 3.14、SQLite 3.46.1）：

| 项目 | 结果 | 阈值 |
| --- | ---: | ---: |
| 20MiB 三域完整写入 P95 | 151.9ms | ≤400ms |
| 20MiB 按版本读取 P95 | 22.6ms | ≤150ms |
| 180 个版本、约 150MiB 原数据全量三域对账 | 0.64s | ≤180s |
| 对账与清理并发时服务进程峰值 RSS | 31.4MiB | ≤250MiB |

共写入 471,859,200 字节三域副本（约 450MiB），对账检查 561 个域副本，未产生差异。Windows 路径全部使用 `pathlib` 和标准库，可用同一脚本在目标 Windows 主机复核。

## 本地故障注入接口

以下接口用于本地验收和测试，不属于生产控制面，也不提供鉴权：

- 写入头 `X-Simulate-Domain-Failure: replica-b`：模拟域写入失败。
- 清理请求体 `fail_domains` 或同名头：模拟域删除失败。
- `PUT /api/v1/admin/versions/<version_id>/retention`：分别设置元数据截止时间、锁截止时间、封存标记和 `lock_pinned`。
- `POST /api/v1/admin/versions/<version_id>/domain-difference`：使用 `{"domain":"replica-b","mode":"missing|truncate|corrupt"}` 制造对账差异。

## 稳定错误码

| 错误码 | HTTP | 场景 |
| --- | ---: | --- |
| `RETENTION_POLICY_SHORTENING_REJECTED` | 409 | 新保留期短于对应类别已生效值 |
| `RETENTION_CONFLICT_ACTIVE` | 409 | 元数据与锁冲突，较晚截止时间仍在未来 |
| `RETENTION_ACTIVE` | 409 | 唯一截止时间仍在未来 |
| `LEGAL_HOLD_ACTIVE` | 409 | 法定封存，无条件拒绝 |
| `LOCK_RECORD_MISSING` | 409 | 锁记录缺失，保守拒绝 |
| `CLEANUP_IN_PROGRESS` | 409 | 同一版本正在清理 |
| `OBJECT_VERSION_NOT_FOUND` | 404 | 版本不存在 |
| `OBJECT_VERSION_DELETED` | 410 | 版本已完成删除 |
| `OBJECT_CONTENT_UNAVAILABLE` | 502 | 无可用域副本 |
| `CLEANUP_ALREADY_RUNNING` | 409 | 已有清理运行 |
| `RECONCILIATION_ALREADY_RUNNING` | 409 | 已有对账运行 |
| `INVALID_TIMESTAMP` | 400 | ISO-8601 时间格式无效 |

服务是单机本地服务，不包含 TLS、身份认证、远程复制协议或分布式共识；题述多域通过同机独立目录和独立锁记录模拟。
