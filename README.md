# 备份链校验与可续传还原演练服务

这是一个仅依赖 Python 3.11 标准库与 SQLite 的本地服务。它把备份仓视为只读输入，
在独立的 SQLite 状态库中提交链视图、逐块选定、演练进度和结论。

## 快速开始

```bash
# 1. 生成固定种子仓（默认 120 个分块、约 250 MiB、1 条全量、14 条增量）
python generate_sample.py --root sample-backup --seed 314

# 2. 审计并提交链视图
python -m backup_chain audit \
  --repo sample-backup \
  --db state/chain.sqlite3 \
  --output-dir restore-out

# 3. 对最近可用还原点做演练（示例为序列 14）
python -m backup_chain drill \
  --repo sample-backup \
  --db state/chain.sqlite3 \
  --output-dir restore-out \
  --restore-point 14

# 4. 查看冻结果和最近注入影响
python -m backup_chain status \
  --repo sample-backup \
  --db state/chain.sqlite3 \
  --output-dir restore-out \
  --audit-id 1 \
  --restore-point 14

# 5. 启动 HTTP 与 Prometheus 端点
python -m backup_chain serve \
  --repo sample-backup \
  --db state/chain.sqlite3 \
  --output-dir restore-out \
  --host 127.0.0.1 \
  --port 8765
```

`--db` 和 `--output-dir` 必须位于备份仓之外；否则服务以
`BC_DATABASE_PATH_INSIDE_REPO` 或 `BC_OUTPUT_PATH_INSIDE_REPO` 拒绝启动。

## 样本场景

生成器默认产出 `healthy` 场景：

- 27 个逻辑全量块，`p-full-primary/full-000/` 中是清单摘要对应的副本。
- 迁移时整分区复制出的 `p-full-legacy/full-000/` 也声明为副本，但其优先级为
  `1`（排在正确副本之前）；其中 8 个块是内容摘要不同的旧版本，用于复现
  “默认取旧副本后增量基线不匹配”。
- 14 条日增量共 66 个变更块，每条增量声明应用前逻辑状态摘要
  `baseline_digest`、块序号 `ordinal`、长度、内容摘要和副本优先级。
- 实际文件总数为 120，约 250 MiB；所有文件由 `--seed` 确定性生成。

生成 `incident` 场景会在最后三条增量加入迁移中断故障：

- `inc-012` 一个声明副本完全缺失。
- `inc-013` 一个块被截断，触发长度不符。
- `inc-014` 一个块长度不变但尾部摘要被破坏。

```bash
python generate_sample.py --root sample-incident --profile incident --force
```

生成器在创建前检查目标父目录至少剩余 500 MiB。空间不足时不会开始生成。

## 清单格式

核心字段如下：

```json
{
  "schema_version": "backup-chain/v1",
  "chain_id": "payments-weekly-chain",
  "block_file_suffix": ".chunk",
  "injection_log": "injections.json",
  "full": {
    "backup_id": "full-000",
    "expected_state_digest": "...",
    "blocks": [
      {
        "block_id": "blk-0001",
        "ordinal": 1,
        "length": 1153433,
        "sha256": "...",
        "tail_sha256": "...",
        "replicas": [
          {"path": "p-full-legacy/full-000/blk-0001.chunk",
           "partition": "p-full-legacy", "priority": 1},
          {"path": "p-full-primary/full-000/blk-0001.chunk",
           "partition": "p-full-primary", "priority": 2}
        ]
      }
    ]
  },
  "increments": [
    {
      "sequence": 1,
      "backup_id": "inc-001",
      "baseline_digest": "...",
      "state_digest": "...",
      "blocks": ["与 full.blocks 相同结构的变更块"]
    }
  ]
}
```

逻辑状态摘要的计算输入是按 `(ordinal, block_id)` 排序的
`[block_id, ordinal, length, sha256]` 列表的规范 JSON。物理路径、副本优先级和
审计时间不参与逻辑摘要，因此同一逻辑状态可由不同分区布局还原出相同摘要。

## 版本选定与链规则

- 只有清单明确声明的副本才可被选为有效版本。目录中额外出现的副本会被索引并
  报告为 `BC_MANIFEST_UNDECLARED_COPY`，但不会把声明副本缺失的块“救回”为可用。
- 候选必须同时满足长度、尾摘要（清单声明时）和完整内容摘要。
- 多个候选都满足清单摘要时，按 `priority` 升序、物理路径升序选定；选定路径、
  分区和优先级全部写入 `restore_point_blocks`。
- 同一逻辑块存在多个不同完整内容摘要时，即使其中一个匹配清单，也记录
  `BC_BLOCK_CONFLICT`，不会静默忽略冲突副本。
- 全量后按 1..14 连续序号累积。当前增量的每个块都必须有唯一可确认版本，才加入
  可用还原点；否则在第一个失败增量/块终止链。还原点 `0` 表示可用全量基线。
- 增量应用前必须满足上一逻辑状态摘要；块序号在增量中不可改变。

主要稳定错误码：

| 错误码 | 含义 |
| --- | --- |
| `BC_BLOCK_MISSING` | 声明副本缺失/不可读 |
| `BC_BLOCK_LENGTH_MISMATCH` | 长度不符，判定半写入 |
| `BC_BLOCK_TAIL_MISMATCH` | 尾摘要不符，判定半写入 |
| `BC_BLOCK_DIGEST_MISMATCH` | 完整摘要不匹配 |
| `BC_BLOCK_CONFLICT` | 同一逻辑块存在不同内容摘要副本 |
| `BC_BLOCK_UNCONFIRMED` | 有文件但没有唯一可确认版本 |
| `BC_MANIFEST_UNDECLARED_COPY` | 目录中存在未声明副本 |
| `BC_MANIFEST_DIRECTORY_MISMATCH` | 清单路径声明与目录结构不一致 |
| `BC_CHAIN_BASELINE_MISMATCH` | 增量基线摘要与已选逻辑状态不符 |
| `BC_CHAIN_SEQUENCE_GAP` | 增量序号不连续 |
| `BC_DRILL_BLOCK_DIGEST_MISMATCH` | 演练期块摘要不符 |

错误对象统一包含 `chain_id`、`backup_id`、`block_id`、`sequence`、`path` 和
`detail`，空值也保留字段，便于稳定解析。

## 演练与续传

演练输出是一个二进制文件，按还原点逻辑状态的 `(ordinal, block_id)` 顺序拼接
已选块。块校验和写入是同一次单向读取：

1. 读取源块并同时计算 SHA-256、写入 `.staging/<drill_id>.part`。
2. 长度和摘要匹配后 `fsync`，再在 SQLite 中提交该块进度。
3. 任一块失败时截断该块在暂存文件中的字节，记录期望/实际摘要和块标识，
   不创建最终输出。
4. 全部块通过后才复制成临时最终文件并原子替换正式输出；最后一块的进度和完成
   结论在同一 SQLite 事务提交。

续传时只读取未提交块。已完成块数和进度位点来自 `drill_blocks` 与
`drills.last_progress_json`，不会重新读取或重写。完成记录按确定性 `drill_id`
复用，后续同标识请求返回 `idempotent_replay: true`，不会覆盖结论。

测试环境可用以下变量在提交指定块数后模拟中断：

```bash
BACKUP_CHAIN_TEST_INTERRUPT_AFTER_BLOCKS=7 \
python -m backup_chain drill ... --restore-point 14
```

首次运行返回 `interrupted`（CLI 退出码 75）；再次运行会从第 7 块之后继续。

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/healthz` | 存活检查 |
| `POST` | `/api/v1/audit` | 重新扫描并提交新链视图 |
| `GET` | `/api/v1/audit?chain_id=&audit_id=` | 查询已提交审计 |
| `GET` | `/api/v1/status?restore_point=&audit_id=&chain_id=` | 指定还原点状态 |
| `POST` | `/api/v1/drills` | `{"restore_point": 14, "audit_id": 1}`，运行/续传 |
| `GET` | `/api/v1/drills?drill_id=` | 查询演练记录 |
| `GET` | `/api/v1/chains` | 各链最新视图 |
| `GET` | `/metrics` | Prometheus 文本格式 |

所有 JSON 响应使用字典序字段输出，列表按 `sequence`、`ordinal`、`block_id`
排序。状态接口固定返回：

- `restore_point`：冻结果摘要、块数与逻辑状态摘要。
- 显式查询不可用序号时返回 `usable: false` 和空 `selected_blocks`，同时保留
  `breakpoint`，不会把断点后的序号伪装成可还原。
- `selected_blocks`：逐块路径、优先级、长度和内容摘要。
- `breakpoint`：首个断点位置与错误码。
- `drill`：进度块数/字节、续传次数、最近进度位点、结论或失败详情。
- `recent_injection_impact`：最近注入事件及受影响块的观测状态。

## Prometheus 指标

标签 `backup_id` 取备份链标识；每次审计只更新指标值，不改变标签集合：

```text
backup_chain_increments_total{backup_id="..."}
backup_chain_usable_restore_points{backup_id="..."}
backup_chain_conflict_blocks{backup_id="..."}
backup_chain_missing_blocks{backup_id="..."}
backup_chain_half_written_blocks{backup_id="..."}
backup_chain_committed_drills_total{backup_id="..."}
```

可用还原点数包含全量基线 `0`。已提交演练数统计该链所有已完成的演练记录。

## SQLite 持久化

主要表：

- `chain_audits`：不可变的链审计版本、指标、断点和错误快照。
- `audited_blocks`：每个备份块的候选、状态与错误码。
- `restore_points` / `restore_point_blocks`：可用序号与逐块冻结选定。
- `injection_impacts`：每次审计观测到的最近注入影响。
- `drills` / `drill_blocks` / `drill_failures` / `drill_events`：进度、续传
  位点、失败期望/实际摘要和事件。

新审计总是创建 `view_version + 1`，不会更新旧审计或旧选定。审计的全部视图、
还原点、逐块选定和错误在一个 `BEGIN IMMEDIATE` 事务中提交。演练的每个进度点
也使用事务提交；最后一个块与完成结论在同一事务中提交。

## 故障注入

注入工具与只读服务分离。它只应由运维/测试显式执行：

```bash
# 同长度内容替换：下次审计断点变为 BC_BLOCK_DIGEST_MISMATCH
python inject_sample.py --repo sample-backup --mode replace --sequence 4

# 截断：判定半写入
python inject_sample.py --repo sample-backup --mode truncate --sequence 5

# 只破坏尾部，保持长度
python inject_sample.py --repo sample-backup --mode tail --sequence 6

# 删除声明副本
python inject_sample.py --repo sample-backup --mode missing --sequence 5

# 反转全量副本优先级，重审后仍应选摘要正确副本
python inject_sample.py --repo sample-backup --mode priority --sequence 0

# 批量复制分区，产生未声明重复副本
python inject_sample.py --repo sample-backup --mode duplicate --sequence 0
```

所有模式会追加 `injections.json` 事件；该文件由服务只读加载，用于状态接口的
“最近一次注入影响”。

## 验证与资源结果

在本工作区以 Python 3.14（代码兼容 Python 3.11）和 250 MiB 样本实测：

| 场景 | 结果 |
| --- | --- |
| 健康链审计 | 0.32 s，峰值 RSS 约 30 MiB |
| 14 条增量全部可用 | 可用点 `0..14`，8 个冲突块显式记录 |
| 序列 14 一次完成演练 | 0.18 s，输出 57,252,238 B，27 块逐块摘要全匹配 |
| 第 7 块后中断再续传 | 只再读 20 块；`resume_count=1`、`blocks_skipped=7` |
| 续传与一次完成对比 | 输出文件 SHA-256 完全一致 |
| 只读检查 | 审计+演练前后仓内 123 个文件长度/mtime/SHA-256 不变 |
| `incident` 样本 | `inc-012` 缺失块处断链，可用点 `0..11` |
| 替换/截断/尾部破坏 | 分别给出摘要、长度、尾摘要错误及首个断点 |
| 先复制分区再删声明副本 | 未声明副本存在，但该块仍保持 `missing` |

自动化回归：

```bash
python -m unittest discover -s tests -v
```

机器可读的实测摘要见 `artifacts/validation-summary.json`。

## 设计取舍

- 不使用内存映射或一次加载整个块；哈希和演练均为 1 MiB 流式处理。
- 不依赖外部数据库、HTTP 框架、Prometheus 服务或网络。
- 服务进程内最多一个审计和一个演练并发执行；跨进程另有标准库文件锁，避免同一
  SQLite 进度或输出目录被并发写入。
- 所有临时/最终输出都位于 `--output-dir`，不会写入备份仓。
- `BACKUP_CHAIN_TEST_INTERRUPT_AFTER_BLOCKS` 仅用于可重复验证中断恢复，不影响
  正常路径。
