# 验证结果

## 环境说明

本次实际执行环境是 Windows 开发机上的 WSL2 Ubuntu：

```text
Linux ... microsoft-standard-WSL2 x86_64
go version go1.22.12 linux/amd64
native go env GOOS=linux
```

因此原生 `go test` / `go run` 在 Linux 目标执行；同时已用纯 Go SQLite 驱动完成 Windows/amd64 交叉编译：

```text
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
exit=0

out/windows/battlekernel.exe: PE32+ executable (console) x86-64
out/windows/review.exe:       PE32+ executable (console) x86-64
```

在原生 Windows PowerShell 中，`go env GOOS` 应为 `windows`；`scripts/verify-windows.ps1` 会显式检查这一点并运行 vet、test、exe 验收和独立复核。

## 已执行命令

以下命令均实际运行：

```bash
go mod tidy
go vet ./...
go test -count=1 ./...
go build -o out/battlekernel ./cmd/battlekernel
./out/battlekernel verify
go run ./cmd/review \
  --db out/data/battle-001.realtime.db \
  --truth out/truth-fingerprints.json \
  --project-id battle-001
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o out/windows/battlekernel.exe ./cmd/battlekernel
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o out/windows/review.exe ./cmd/review
```

退出码：

```text
go vet     = 0
go test    = 0
go build   = 0
verify     = 0
review     = 0
windows    = 0,0
```

## 全量测试

```text
?       battlekernel/cmd/battlekernel   [no test files]
?       battlekernel/cmd/review         [no test files]
?       battlekernel/internal/canonical  [no test files]
?       battlekernel/internal/checkpoint [no test files]
?       battlekernel/internal/errs       [no test files]
?       battlekernel/internal/model      [no test files]
?       battlekernel/internal/review     [no test files]
?       battlekernel/internal/sample     [no test files]
?       battlekernel/internal/sched      [no test files]
?       battlekernel/internal/web        [no test files]
ok      battlekernel/internal/app        0.059s
ok      battlekernel/internal/convert    0.001s
ok      battlekernel/internal/engine     0.004s
ok      battlekernel/internal/loader     0.005s
ok      battlekernel/internal/pipeline   0.010s
```

额外执行 `go test -race -count=1 ./internal/sched ./internal/app ./internal/pipeline`，退出码为 0。

测试覆盖：

- 对齐/非对齐整型换算、高格式→低格式→高格式顺序哈希不变、`E_OVF` 元数据。
- 三套格式映射、整数字段和列表字段互换、未知格式与缺失字段路径。
- 三样本真值、`E_ORDER`、`E_STATE`、`E_FLAG`、`E_COOL`。
- 实时/断点/全量三路径一致。
- SQLite 回合哈希篡改后返回 `E_CHECKPOINT` 并整体重置恢复。
- 一键验收报告整体 `ok=true`。

## 验收命令实际输出

```text
battlekernel verification: ok=true samples=3 seed=20261125 window=2
  [PASS] load-set:battle-001 formats=tick-weight-v1,chrono-swapped-v2,compact-legacy-v3
  [PASS] load-set:battle-002 formats=tick-weight-v1,chrono-swapped-v2,compact-legacy-v3
  [PASS] load-set:battle-003 formats=tick-weight-v1,chrono-swapped-v2,compact-legacy-v3
  [PASS] integer-roundtrip floor ms -> round=3 canonical_ms=1760000002000
  [PASS] overflow-e_ovf E_OVF field=probe.round: integer conversion overflow
  [PASS] corrupt-engineering-e_decode E_DECODE field=$
  [PASS] unknown-format-e_schema E_SCHEMA field=format: unknown format number: 99
  [PASS] missing-required-e_schema E_SCHEMA field=battle_id
  [PASS] flag-contradiction-e_flag E_FLAG field=timePoints[0].commands[2].conditionFlag round=1 unit=u3
  [PASS] cooldown-bound-e_cool E_COOL field=units[id=u1].recast unit=u1 value[0]=101 value[1]=99
  [PASS] runtime-cooldown-e_cool-round-isolated E_COOL field=state.cooldown_remaining round=1 unit=u1 skill=s1-heavy
  [PASS] weight-edit-invalidation first=3 invalidations=12 compare=true
  [PASS] insert-reorder-invalidation first=4 invalidations=6 compare=true
  [PASS] ineffective-edit-reuses-all no semantic change; persistent snapshots retained
  [PASS] bounded-buffer-blocking wait_events=1 max_wait_ms=30 reason=buffer-exhausted-should-wait
  project battle-001 truth=true realtime=true resume=true failure_retry=true snapshot_reset=true review=true
  project battle-002 truth=true realtime=true resume=true failure_retry=true snapshot_reset=true review=true
  project battle-003 truth=true realtime=true resume=true failure_retry=true snapshot_reset=true review=true
  weight invalidation: first_round=3 invalidations=12 reused=[1 2] recomputed=[3 4 5 6]
  reorder invalidation: first_round=4 invalidations=6 reused=[1 2 3] recomputed=[4 5 6]
  report: out/reports/verification-report.json
```

## 独立复核工具实际输出

```json
{
  "ok": true,
  "db_path": "out/data/battle-001.realtime.db",
  "project_id": "battle-001",
  "rounds_checked": 6,
  "snapshot_hashes_ok": true,
  "truth_fingerprints_ok": true
}
```

```text
REVIEW_OK: snapshot hashes, independent recalculation and truth fingerprints match
```

## 本地页面实测

```text
$ ./out/battlekernel serve --addr 127.0.0.1:18080
serving local page at http://127.0.0.1:18080

GET  /api/health  -> {"ok":true,"service":"battlekernel"}
POST /api/verify  -> HTTP 200, report.ok=true, projects=3
GET  /            -> 返回内置 HTML 页面
```

## 产物

- `out/samples/*.format1.json`、`*.format2.json`、`*.format3.json`：三份发行格式。
- `out/samples/*.truth.json`：完整真值。
- `out/truth-fingerprints.json`：逐回合真值指纹和总指纹。
- `out/data/*.db`：SQLite 检查点数据库。
- `out/reports/verification-report.json`：含依赖图、失效清单、调度指标和三路径比对的完整报告。
- `out/windows/*.exe`：Windows/amd64 纯 Go 可执行文件。
