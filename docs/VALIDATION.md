# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

```text
go version go1.22.12 linux/amd64
GOMODCACHE=/home/liang/.feedback/runtime-home/.../go/pkg/mod
CGO_ENABLED=0
GOPROXY=off
```

所有 Go 验证均使用 `CGO_ENABLED=0 GOPROXY=off`，只读取本地模块缓存，不联网下载依赖。SQLite 驱动为纯 Go 的 `modernc.org/sqlite v1.34.1`。

## 已执行命令

```bash
go mod verify
CGO_ENABLED=0 GOPROXY=off go vet ./...
CGO_ENABLED=0 GOPROXY=off go test -count=1 ./...
bash scripts/reproduce.sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOPROXY=off go build ./...
```

结果：

- `go mod verify`：`all modules verified`。
- `go vet ./...`：通过，无输出。
- `go test -count=1 ./...`：全部通过。
  - `internal/dataset`：通过（正常加载 + 5 类错误码/字段/帧号用例）。
  - `internal/verify`：通过（参数边界、置信度单调性、缓冲排队、差异定位、解码确定性、三段检测、三路径、参数失效、故障重试、版本作废）。
  - `internal/review`：通过（持久化摘要篡改定位）。
- `bash scripts/reproduce.sh`：退出码 0。
- Windows `GOOS=windows GOARCH=amd64` 交叉编译：通过，生成 `verify.exe`、`review.exe`、`gensample.exe`。

## 本地样本检测结果

固定种子 `334462`，每段 16 帧、96×64、RGB、块大小 16。

| 片段 | 真值替换区间 | 核验/复核重定位 | 结论 |
| --- | ---: | --- | --- |
| `noise` | 8–10 | 8–11 | 容差 1 内匹配；第 11 帧是替换块恢复边界 |
| `watermark` | 7–9 | 7–9 | 精确匹配，分别覆盖缺失、错号、错位 |
| `clean` | 无 | 无 tampered 帧、无区间 | 无误报 |

独立复核工具对三段均输出 `"ok": true`；每段检查 96 条证据、384 个噪声块和 4 个检查点。结果文件：

- `artifacts/review-noise.json`
- `artifacts/review-watermark.json`
- `artifacts/review-clean.json`

## 三路径一致性

实验命令使用 `-parallelism 4 -cancel-after 2`，即恢复路径在 2 个已提交批后模拟停止/重启。

| 片段 | 实时/恢复/全量 | 恢复信息 | 实际提交条目数 |
| --- | --- | --- | ---: |
| `noise` | 一致 | recovered=true, completed_checkpoints=2, loaded_entries=48 | 96 / 96 / 96 |
| `watermark` | 一致 | recovered=true, completed_checkpoints=2, loaded_entries=48 | 96 / 96 / 96 |
| `clean` | 一致 | recovered=true, completed_checkpoints=2, loaded_entries=48 | 96 / 96 / 96 |

自动比较通过：逐帧判决、逐块噪声统计、替换区间、最终缓存键提交序列、SQLite `seq,ordinal` 实际提交序列均无差异。完整 JSON：`artifacts/experiment-report.json`。

恢复路径统计为重算 48、命中 48：前两批从 SQLite 恢复，后两批继续计算。

## 参数调整后的精确失效

参数实验先把初始 96 个证据写入 SQLite，关闭并重新打开数据库、执行 `Recover`，再调整参数。每帧 6 个证据节点，共 96 个。

| 调整 | 重算/命中 | 失效节点 | 原因统计 |
| --- | ---: | --- | --- |
| `block_size: 16 -> 8` | 32 / 64 | 16 `noise_variance` + 16 `decision` | 16 `param:block_size`，16 `upstream:noise_variance` |
| `tamper_threshold: .55 -> .80` | 16 / 80 | 16 `decision` | 16 `param:tamper_threshold` |
| 修正 `watermark_region` | 48 / 48 | 16 水印 + 16 时间轴 + 16 判决 | 16 `param:watermark_region`，32 上游传播 |
| `residual_window: 3 -> 2` | 32 / 64 | 16 `residual_energy` + 16 `decision` | 16 `param:residual_window`，16 上游传播 |

增量最终快照与空缓存用新参数全量重算的快照全部一致；`E_DIFF` 比较器有专门测试，可定位到 `frame=8 block_x=3 block_y=0 field=variance`。

## 故障隔离与重试

手动/脚本验证命令注入：

```text
-inject-failure-once 8:noise_variance -retry-failed=true
```

最终报告统计：

```json
{"recompute_count": 96, "hit_count": 0, "parallelism": 4,
 "failure_count": 1, "retry_count": 1}
```

失败节点先写为 SQLite `status='failed'`，不进入检查点批；重试成功后替换为 `ok`。故障恢复后的快照、提交序列和独立复核结果均与无故障路径一致。

## 异常码抽查

非整除分块：

```bash
go run ./cmd/verify run -sample data/sample/noise -block-size 7 ...
```

返回退出码 2 和结构化错误：

```json
{"code":"E_PARA","field":"block_size","message":"block_size must divide frame width and height"}
```

加载器测试还覆盖：

- 清单缺失/帧截断：`E_DECODE`
- `width`、`frame_count` 不符：`E_META` + 字段名
- 时间戳倒置：`E_ORDER` + 首个违规帧号 2
- 水印区域越界：`E_RANGE` + 帧号 1

## 产物

- 样本：`data/sample/{noise,watermark,clean}/truth.json`
- 单次报告：`artifacts/report-{noise,watermark,clean}.json`
- 独立复核：`artifacts/review-{noise,watermark,clean}.json`
- 三路径/注入总报告：`artifacts/experiment-report.json`
- SQLite：`artifacts/*.db` 与 `artifacts/experiment-dbs/*.db`

强制终止不会生成可信完整报告；只有 SQLite 中已事务提交且版本/内容指纹匹配的批可在重启后恢复。
