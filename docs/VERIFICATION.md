# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

- Go：`go1.22.12 linux/amd64`，满足 Go 1.22+。
- `go env GOMODCACHE`：`/home/liang/.feedback/runtime-home/.../go/pkg/mod`，可写。
- SQLite：vendored pure-Go 驱动 `modernc.org/sqlite v1.29.10`。
- `CGO_ENABLED=0`；不使用本机失效的 gcc，也不要求 Windows 安装 mingw-w64。
- 未使用 Docker，未访问真实账号、线上站点或未授权目标。

## 命令与结果

```bash
go vet ./...
go test -count=1 ./...
./scripts/acceptance.sh
```

实际结果：

- `go vet ./...`：通过。
- `go test -count=1 ./...`：`terrain` 包通过，三个命令均成功编译。
- 端到端验收：`31` 项检查全部 `PASS`，`0` 项失败。
- 验收 JSON：`artifacts/acceptance/acceptance-report.json`。

## 样本网格摘要

固定种子 `42` 生成 4 个块。一次性全量路径的实测规模：

| 块 | 顶点 | 三角形 | 最大高程误差 |
|---:|---:|---:|---:|
| 0 | 45 | 69 | 0.175538 |
| 1 | 74 | 118 | 0.139647 |
| 2 | 71 | 118 | 0.158956 |
| 3 | 172 | 305 | 0.119969 |

24 个真值格点全部成为网格顶点，高度偏差未超过对应声明容限；各块最大误差
未超过真值清单中的声明上界。容差序列 `2.0 → 0.5 → 0.1 → 0.02` 收紧时，
三角形数单调不减，最终为 754。

## 已验证的关键断言

- `E_SCHEMA`：缺少 `blocks[2].sample_budget`、共享边采样列长度不一致、
  `shared_boundaries[0].constraint_id` 引用缺失，均返回结构化错误路径和块编号。
- 确定性：16 个并发重复构建结果与基准网格逐字段一致；顶点和三角形均有规范排序。
- 断裂归因：`fault-ok`、`fault-undersample`、`fault-alignment-order`、
  `fault-align-unavailable` 分别得到 `ok`、`undersample`、
  `alignment_mismatch`、`alignment_unavailable/E_ALIGN`。
- 预算：并行槽耗尽记录 `max_parallel` 排队原因；内存预算耗尽记录
  `mesh_memory_budget`；超过单线程顶点缓冲上限返回 `E_BUDGET`。
- 失效传播：修改块 1 的采样预算后，提交顺序为 `[0,1,3]`，块 2 保持
  `persisted_cache_reused`。
- 三路径一致：串行草稿增量、恢复 `[0,1]` 检查点后的断点续算、空缓存一次性全量，
  在预算改为 `48` 的同一编辑工程上顶点、三角形和边界报告逐项相等。
- 提交序列：全量、恢复和串行路径的稳定序列均为 `[0,1,2,3]`。
- 版本控制：写入陈旧算法/参数版本后启动服务，`version_reset=true`，旧快照被
  整体作废并成功全量重剖。
- 异常隔离：首次构建注入 panic 后仅该块失败；重试后网格与全量结果一致，
  其他块和已提交事务未被污染。
- HTTP：实际启动 `cmd/terrain-server` 后，`GET /api/state` 返回 4 个缓存块、
  4 条边界报告和 4 条断裂样例；编辑块 2 预算返回 `commit [0 2 3]`、
  `cache_hits=1`。

## SQLite 实测

使用 Python 标准库 `sqlite3` 可直接打开
`artifacts/acceptance/full.db`，可见表：

```text
blocks, boundary_stats, commit_events, commit_sequence,
dependencies, snapshots, sqlite_sequence
```

`commit_sequence` 中稳定序号为：

```text
0 -> block 0
1 -> block 1
2 -> block 2
3 -> block 3
```

每个 `blocks` 行包含顶点/三角形数量、`vertex_digest`、`triangle_digest`、
`content_hash`、网格 JSON、依赖 JSON、边界统计 JSON、算法版本和参数版本。
