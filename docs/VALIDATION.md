# 实际验证记录

验证时间：2026-11-27（Asia/Shanghai）。

环境：

- Linux / WSL Ubuntu（任务给定）
- `go version go1.22.12 linux/amd64`
- 外部交叉检查工具：Python `3.14.4`，内置 `sqlite3.sqlite_version = 3.46.1`
- 仅使用 Go 标准库与仓库内代码；未使用 Docker、外部数据库服务或第三方 Go 包

## 1. 格式化、vet 与全量测试

命令：

```bash
gofmt -w .
go vet ./...
go test -count=1 ./...
```

实际结果：`go vet` 无输出并成功；全量测试通过。

```text
?   	wardrobekernel/cmd/wardrobe	[no test files]
?   	wardrobekernel/internal/atomicfile	[no test files]
?   	wardrobekernel/internal/cache	[no test files]
?   	wardrobekernel/internal/checkpoint	[no test files]
?   	wardrobekernel/internal/compare	[no test files]
?   	wardrobekernel/internal/hashx	[no test files]
?   	wardrobekernel/internal/model	[no test files]
?   	wardrobekernel/internal/samplegen	[no test files]
?   	wardrobekernel/internal/xerr	[no test files]
ok  	wardrobekernel	0.003s
ok  	wardrobekernel/internal/assembly	0.003s
ok  	wardrobekernel/internal/batching	0.002s
ok  	wardrobekernel/internal/depgraph	0.002s
ok  	wardrobekernel/internal/pipeline	0.012s
ok  	wardrobekernel/internal/scheduler	0.142s
ok  	wardrobekernel/internal/schema	0.002s
ok  	wardrobekernel/internal/sqlitefile	0.005s
```

并发与重复稳定性：

```bash
go test -race -count=1 ./internal/scheduler ./internal/pipeline .
```

```text
ok  	wardrobekernel/internal/scheduler	1.152s
ok  	wardrobekernel/internal/pipeline	1.051s
ok  	wardrobekernel	1.011s
```

另连续执行 3 次 `go test -count=1 ./internal/...`，全部通过。

离线依赖核对：

```bash
GOPROXY=off GOSUMDB=off go build ./...
GOPROXY=off GOSUMDB=off go test -count=1 ./...
```

均成功；仓库没有 `go.sum`，`go.mod` 无任何第三方 `require`。

## 2. 一键验收

命令：

```bash
go run ./cmd/wardrobe verify --dir samples --work artifacts/verify-final
```

实际 JSON 中 `"ok": true`，13 项检查全部通过：

```text
PASS E_SCHEMA field paths — 12 schema violations
PASS E_ARB parties and basis chain — parties=[part_a part_b] basis=6
PASS truth itemwise match — fresh deterministic plan equals checked-in truth
PASS realtime bounded scheduling — 7 wait records, 1 recovered failures, 4 commits
PASS realtime path equals full — batches, offsets, conflicts and commit sequence equal
PASS checkpoint resume equals full — all durable batches reused and compared equal
PASS pure-Go SQLite file readable — 7 k/v rows
PASS E_VER version mismatch — persisted=batch-kernel-v1.0.0 expected=batch-kernel-v0.0.0-wrong affected=[1 2 3 4]
PASS precise invalidation — invalid=[3] reused=[1 2 4]
PASS incremental equals full — first mismatch locator is available on E_DIFF
PASS SIGKILL commit boundary — signal=killed durable_batches=2 run_error=signal: killed
PASS post-SIGKILL recovery equals full — restart reused batches 1-2 and recomputed tail
PASS batch budgets — all batch vertex/index/slot totals within constraints
```

## 3. 固定样本结果

命令：

```bash
go run ./cmd/wardrobe generate --dir samples --seed 20261127
go run ./cmd/wardrobe validate --project samples/project.json --truth samples/truth.json
```

实际批次：

```text
batch 1 ['face_base', 'chest_plate', 'hair_long'] v=1060 i=1848 slots=4
batch 2 ['cape_red', 'pauldron_l', 'boot_pair', 'gauntlet_pair'] v=1080 i=1710 slots=3
batch 3 ['waist_belt'] v=140 i=210 slots=1
batch 4 ['greave_l'] v=300 i=480 slots=1
```

冲突类别包含：

```text
ATTACH_POINT_CONTENTION
EXPLICIT_MUTEX
SKIN_INCOMPATIBLE
```

## 4. 增量失效与 E_DIFF

材质槽编辑：

```bash
go run ./cmd/wardrobe incremental-demo \
  --cache artifacts/demo/cache-material.json
```

实际结果：

```text
ok=True invalid=[3] reused=[1 2 4] equal=True
msg=material waist_belt/main -> v2-edited
```

几何规模编辑：

```bash
go run ./cmd/wardrobe incremental-demo \
  --cache artifacts/demo/cache-geometry.json \
  --edit-part waist_belt --vertex-delta 24
```

实际结果：

```text
ok=True invalid=[3] reused=[1 2 4] equal=True
msg=geometry waist_belt vertices += 24
```

首差异定位诊断：

```bash
go run ./cmd/wardrobe diff-demo
```

实际退出码为 `1`，JSON 为：

```json
{
  "code": "E_DIFF",
  "message": "incremental and full recompute differ",
  "first_mismatch": {
    "batch_number": 1,
    "part_id": "face_base",
    "field_name": "submesh.vertex_start",
    "path": "submeshes.sm_face",
    "expected": 0,
    "actual": 7
  }
}
```

## 5. 异常码核对

`E_SCHEMA`：

```bash
go run ./cmd/wardrobe validate --project samples/invalid_schema.json
```

实际返回 `code=E_SCHEMA`，共 12 条字段级违规，首条路径包含 `attach_points[1].id`；部件区间问题带有 `part_id=p_bad_ref` 与 `parts[0].submeshes...` 路径。

`E_ARB`：

```bash
go run ./cmd/wardrobe demo --project samples/ambiguous_arb.json --truth=
```

实际返回 `code=E_ARB`，`parties=["part_a","part_b"]`，`basis_chain` 包含约束版本、争用类型、双方优先级、平局值和 `tie_policy=strict`。

`E_VER`：

```bash
go run ./cmd/wardrobe version-check \
  --checkpoint <resumed.sqlite> --algorithm-version wrong-v
```

实际返回 `code=E_VER`，并列出 `affected_batches=[1,2,3,4]`、持久化版本与期望版本。

## 6. 强制终止与 SQLite 外部校验

手动复现命令：

```bash
db=artifacts/demo/kill-manual.sqlite
go build -o artifacts/bin/wardrobe ./cmd/wardrobe
./artifacts/bin/wardrobe demo \
  --checkpoint "$db" --kill-after-commit 2 \
  --work-delay-ms 0 --fail-first ''
# shell: exit code 137 / SIGKILL

python3 - "$db" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
print(con.execute("select count(*) from records where k like 'batch:%'").fetchone()[0])
PY

./artifacts/bin/wardrobe resume --checkpoint "$db"
```

实际结果：

```text
kill_rc=137
durable after kill: 2
resume: True — resumed from 2 durable batches; committed 2 tail batches
comparison.equal: True
durable keys after resume:
  batch:000001, batch:000002, batch:000003, batch:000004
```

Python `sqlite3` 可直接打开纯 Go 写出的文件：

```text
schema = [('table', 'records', 2, 'CREATE TABLE records(k TEXT, v TEXT)')]
rows = [
  ('batch:000001', 2958), ('batch:000002', 3022),
  ('batch:000003', 1233), ('batch:000004', 1213),
  ('meta.algorithm_version', 19), ('meta.constraint_version', 21),
  ('meta.schema', 29)
]
```

`file` 也识别为：

```text
SQLite 3.x database ... page size 65536 ... database pages 2
```

另外用 160 行、约 256 KiB 的检查点验证了多叶 B-tree 分页；Python 结果为：

```text
PRAGMA integrity_check = ok
count(*) = 160
```

## 结论

本地固定样本已验证：合批划分、子网格偏移、三类装配冲突、仲裁链、增量精确失效、三路径逐项一致、异常码、SQLite 原子检查点和 SIGKILL 恢复边界均符合首版验收口径。
