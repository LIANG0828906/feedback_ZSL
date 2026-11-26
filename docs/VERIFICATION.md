# 实际验证记录

验证时间：2026-11-27（Asia/Shanghai）。

环境：

```text
go version go1.22.12 linux/amd64
```

实际工具链为 Go 1.22.12，`go.mod` 声明 `go 1.21`；实现只使用标准库，没有第三方依赖。

## 单元与端到端测试

命令：

```bash
go test -count=1 ./...
```

实际结果：

```text
?   	mirrorcast/cmd/mirrorcast	[no test files]
?   	mirrorcast/internal/acceptance	[no test files]
?   	mirrorcast/internal/bizerror	[no test files]
?   	mirrorcast/internal/checkpoint	[no test files]
?   	mirrorcast/internal/engine	[no test files]
?   	mirrorcast/internal/generator	[no test files]
?   	mirrorcast/internal/incremental	[no test files]
ok  	mirrorcast	0.020s
?   	mirrorcast/internal/verify	[no test files]
ok  	mirrorcast/internal/projectio	0.008s
ok  	mirrorcast/internal/safemath	0.002s
ok  	mirrorcast/internal/session	0.002s
```

`go vet ./...` 无输出并以退出码 0 完成。

## 固定种子与确定性

生成命令：

```bash
go run ./cmd/mirrorcast generate --out samples/generated
```

实际结果：

```text
generated 5 projects with seed 0x42A8691E (1118333214) in samples/generated
verification: ok=true projects=5 critical_cases=14
```

同一种子分别生成到两个全新临时目录后，`diff -r <dir-a> <dir-b>` 没有差异，结论为：

```text
BYTE_IDENTICAL
```

目录复核：

```bash
go run ./cmd/mirrorcast verify --dir samples/generated
```

实际结果：

```text
verify samples/generated: ok=true projects=5 critical=14
  01-canonical versions=true truth=true precision_items=20
  02-random-1 versions=true truth=true precision_items=36
  03-random-2 versions=true truth=true precision_items=57
  04-random-3 versions=true truth=true precision_items=37
  05-random-4 versions=true truth=true precision_items=42
```

每个工程的 v1/v2/v3 装载语义等价，三版重放均与导出的逐时刻真值一致。精度项的矩形、平面尺寸和预算差均未超过声明容差 `rectangle=0px, plane=0px, budget=0kbps`。

主要产物：

- `samples/generated/*.v1.json`、`*.v2.json`、`*.v3.json`
- `samples/generated/*.truth.json`
- `samples/generated/critical-cases.json`
- `samples/generated/precision-report.json`
- `samples/generated/verification-report.json`
- `samples/generated/manifest.json`

## 端到端验收

命令：

```bash
go run ./cmd/mirrorcast acceptance --work artifacts/acceptance
```

实际结果：

```text
acceptance: ok=true checks=13
  [PASS] checkpoint recovery and step resume
  [PASS] screen state precedes global tick marker
  [PASS] truncated checkpoint code (E_CHECKPOINT_TRUNCATED)
  [PASS] version mismatch forces full recompute (E_CHECKPOINT_VERSION_MISMATCH)
  [PASS] canonical sorting makes settlement order-independent
  [PASS] leave releases budget without moving peers
  [PASS] canonical downgrade and rejection constraints
  [PASS] chroma edit invalidates only dependent suffix
  [PASS] scale edit invalidates dependent suffix
  [PASS] budget edit recomputes settlement suffix
  [PASS] delete topology event recomputes event suffix only
  [PASS] append topology event extends recomputation interval
  [PASS] invalid incremental scale code (E_SCALE_ZERO)
```

完整 JSON 报告位于 `artifacts/acceptance/acceptance-report.json`。

## Canonical 样本关键结论

- tick 3：`s03` 请求档超预算，依据 `C-BUDGET-TOTAL` 降到 `3750/10000`。
- tick 4：`s04` 最低可接受档仍无法放入剩余预算，依据 `C-MIN-QUALITY` 返回 `E_ADMISSION_REJECTED`。
- tick 6：`s02` 退出并释放预算；`s01`、`s03` 的矩形、平面尺寸和费用与 tick 5 完全相同。
- tick 8：总预算下调后严格落到相邻声明刻度：`s03=2500`、`s04=5000`、`s01=6250`；`used_budget_kbps=39668`，`remaining_budget_kbps=5332`。
- 检查点从 tick 3 恢复后，tick 4-8 与确定性真值逐字段一致；恢复状态中的屏幕 ID 唯一且费用汇总等于已用预算。
