# 本地验证记录（可重复）

环境：WSL Ubuntu，Go 1.22.12 linux/amd64，仅标准库。

## 复现命令

```bash
go vet ./...
go test ./...
go test -race ./internal/runtime/... ./internal/engine/...
go run ./cmd/schedlab all artifacts/demo
```

也可单独运行子命令：`load | feasibility | score | scale | incremental | checkpoint | faults`。

## 实测结论（固定种子 20261127）

- 三版本装载：standard 与 large 两个工程经 v1/v2/v3 装载后节点集合、Pod 集合、
  规则绑定与遍历次序完全一致（报告 `versions_equal=true`，diff 为空）。
- 可行性：每个 Pod 输出可行节点集合与每个淘汰节点的全部失败约束（C1..C5，声明次序），
  可行+淘汰恰好覆盖全部节点。
- 打分：所有候选节点归一化分位于 `[0,10000]` bps；引擎两路径与独立 oracle 的
  原始分、归一化分、分项贡献全部一致；standard 走 `EXACT_NODEWISE`，large 中乘积达
  52（>40）的 Pod 走 `TOPOLOGY_PREAGGREGATED`，两路径选点相同。
- 换算审计：13 个临界用例全部在声明容差内（`all_within_tolerance=true`），
  非整数千分比按预期拒绝；等比缩放不改变排序。
- 增量：四类编辑按序施加后与一次性全量重算零分歧；实测缓存
  `reused=124 / recomputed=80 / total=96`，并输出每处失效来源与传播闭包。
- 检查点：模拟强制终止后重开存储，已提交帧全部恢复并在重跑时复用
  （`recovery_sound=true`）；版本不匹配返回 `E_STATE_VERSION_MISMATCH`，
  截断文件返回 `E_CHECKPOINT_TRUNCATED`。
- 异常注入：八类异常稳定返回
  `E_WEIGHT_RANGE / E_RESOURCE_NEGATIVE / E_TOPO_KEY_CONFLICT /
   E_CONCURRENCY_BUDGET / E_BUFFER_BUDGET / E_BACKPRESSURE /
   E_CHECKPOINT_TRUNCATED / E_STATE_VERSION_MISMATCH`；
  源工程 JSON 校验和在注入前后不变。
- 并行：`go test -race` 无数据竞争；剔除耗时观测字段后两次运行打分结果逐字节一致，
  耗时字段仅作诊断、不参与正确性判定。

## 演示产物

`artifacts/demo/` 下生成：`report.json`（装载/可行性/打分/换算/增量）、
`checkpoint.json` 与 `checkpoint/`（崩溃恢复）、`faults.json`（异常注入）。
