# 实际验证记录

验证时间：2026-11-27 03:38 CST（Asia/Shanghai）。

## 环境

```text
$ go version
go version go1.22.12 linux/amd64
```

`go.mod` 声明 `go 1.21`；实现只使用 Go 标准库，没有第三方依赖或 `go.sum` 外部包。

## 全量测试

```text
$ go test -count=1 -v ./...
...
19/19 top-level tests passed
0 failed
```

覆盖：

- 固定种子确定性与生成样本 schema 合法。
- `E_SCHEMA` 的 alpha、normal、重叠、缺失 ID 对象/字段定位。
- 状态机合法/非法迁移、`E_STATE` 保持原状态、失败重试。
- 调度池最大并行度、panic 隔离、队列预算耗尽阻塞记录。
- 检查点提交/恢复、torn tail、算法/参数版本不匹配过滤。
- 固定种子三路径端到端、断点恢复、独立审计。
- 移动声源后增量与空缓存全量一致。
- 1/4 并行度结果一致。
- `E_CONS` 定位频带和来源分块。
- 路径缓冲 `E_BUDGET` 后放大缓冲重试成功。
- 复核器首个分块/频带/字段差异定位。
- 诊断页 HTML 与 `/api/state`。

## Race 与 vet

```text
$ go vet ./...
# exit 0

$ go test -race -count=1 ./...
ok  acousticfield/internal/audit
ok  acousticfield/internal/checkpoint
ok  acousticfield/internal/rebuild
ok  acousticfield/internal/schedx
ok  acousticfield/internal/schema
ok  acousticfield/internal/statefx
ok  acousticfield/internal/web
# exit 0
```

## 端到端三路径

```text
$ bash scripts/demo.sh out/demo
== go version ==
go version go1.22.12 linux/amd64
== unit/integration tests ==
... all packages ok ...
== deterministic three-path demo ==
demo output: out/demo
compare ok=true
audit full     ok=true chunks=9 findings=0
audit realtime ok=true chunks=9 findings=0
audit recovery ok=true chunks=9 findings=0
== independent comparison ==
== independent audits ==
OK: report=out/demo/report.json
SCRIPT_EXIT=0
```

CLI 单独复核结果：

```text
$ go run ./cmd/verify compare out/demo/full out/demo/realtime out/demo/recovery
COMPARE_OK dirs=[out/demo/full out/demo/realtime out/demo/recovery]

$ go run ./cmd/verify audit out/demo/full
AUDIT_OK dir=out/demo/full chunks=9 findings=0
```

## 实际产物抽查

最终实时编辑回合 `out/demo/realtime/report.json`：

```text
invalidations:
  C00 upstream_boundary_energy_changed
  C01 source_emission_changed
  C02 upstream_boundary_energy_changed
  C04 upstream_boundary_energy_changed

commit sources:
  C00 computed, C01 computed, C02 computed, C03 reused,
  C04 computed, C05 reused, C06 reused, C07 reused, C08 reused

conservation ok=true residual_q=[0 0 0]
```

恢复目录 `out/demo/recovery/report.json` 的提交来源：

```text
C00 recovered, C01 recovered,
C02 computed, C03 computed, C04 computed, C05 computed,
C06 reused, C07 computed, C08 reused
```

这说明强制/取消前已提交的 C00、C01 被恢复，未完成块整块重算，未失效块复用持久化结果。

## 诊断页

```text
$ go build -o /tmp/reverb ./cmd/reverb
$ /tmp/reverb serve -report out/demo/report.json -addr 127.0.0.1:18088
$ curl http://127.0.0.1:18088/healthz
ok
$ curl http://127.0.0.1:18088/api/state
# round=edit-emission chunks=9 conservation.ok=true
```

## 独立性检查

```text
$ go list -deps ./cmd/verify | grep -E 'internal/(rebuild|engine)' || echo no-main-recompute-deps
no-main-recompute-deps
```

`cmd/verify` 仅依赖 `internal/audit`、`internal/checkpoint` 和 `internal/acoustic`；审计器重算逻辑未调用主编排或路径筛选内核。
