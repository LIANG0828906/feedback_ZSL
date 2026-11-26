# 实际验证记录

验证时间：2026-11-27（Asia/Shanghai）

环境：

- WSL Ubuntu 工作区
- `go version go1.22.12 linux/amd64`
- 仅 Go 标准库；`go.mod` 无第三方依赖

## 执行命令

```bash
gofmt -w .
go vet ./...
go test -count=1 ./...
go test -race -count=1 . ./internal/engine ./internal/litedb ./internal/checkpoint
go run ./cmd/screenrelay verify
```

原始输出保存在：

- `artifacts/reports/final-validation.log`
- `artifacts/reports/go-test-verbose-final.log`
- `artifacts/reports/final-race.log`
- `artifacts/reports/verification-report.json`

## 结果摘要

- `gofmt -l .`：无输出。
- `go vet ./...`：通过。
- `go test -count=1 ./...`：通过；根包 9 个验收测试全部 PASS。
- `go test -race ...`：通过，未报告数据竞争。
- CLI `verify`：`ok=true`，32 项检查全部通过。
- 固定种子：`20261127`。
- 三版本样本：`artifacts/samples/session.v1.json`、`session.v2.json`、`session.v3.json`、`artifacts/samples/truth.json`。
- 降级清单：v1 有 34 条 `E_LOSSY` 枚举处置记录；v2/v3 为 0。

## Go 验收测试

```text
PASS TestThreeVersionsLoadAndIntegerRoundTrip
PASS TestMappingBoundsOrderAndErrors
PASS TestThreePathsAndCheckpointRecovery
PASS TestStateMachinesRejectIllegalTransition
PASS TestVerificationSuiteReproducesExceptionChains
PASS TestLowVersionExportRestoresCompatibleTopologyAndCapabilities
PASS TestCompareLocatesFirstMismatch
PASS TestPoolIsolatesTaskErrors
PASS TestNonAlignedResidualsStayInsideBounds
```

## 三路径结果

三条路径均产出：

```text
windows=6
commit_sequence=[1 2 3 4 5 6]
final_screen_ids=[scr-a scr-b scr-d scr-e]
first_mismatch=""
```

断点路径在第 2 窗后中断，重新打开检查点后：

```text
recovery invalid_seq=0
compare_recovery_full=PASS
```

第 3 窗中 `scr-e` 的 replay bundle 来源为第 2 窗；第 6 窗活动集合不含 `scr-c`，也没有其镜像包。
生成器 `truth.json` 中的逐窗活动屏/回放屏真值也与已提交结果一致（`truth_manifest_windows=PASS`）。

## 样本窗口实测规模

以下数据从最终 `artifacts/reports/realtime.checkpoint` 的已提交 payload 提取：

| 窗口 | 活动屏 | 当前包数 | 回放包数 | 缩放整数偏差范围 | 裁剪截断范围(px) |
| --- | ---: | ---: | ---: | --- | --- |
| 1 | 4 | 243 | 0 | 0..25600 | 1796..3794 |
| 2 | 4 | 229 | 0 | 0..51200 | 1806..3778 |
| 3 | 5 | 257 | 6 | 0..38400 | 1810..3838 |
| 4 | 5 | 225 | 0 | 0..25600 | 1800..3742 |
| 5 | 5 | 249 | 0 | 0..25600 | 1808..3798 |
| 6 | 4 | 158 | 0 | 0..25600 | 1814..2730 |

样本真值的坐标为 256 对齐、时间为 50ms 对齐，所以坐标/时间余数为 0；这是合法实测值，且仍小于上界。`TestNonAlignedResidualsStayInsideBounds` 额外构造非对齐输入，验证非零坐标余数、缩放偏差和时间余数均在上界内。

## 异常链路

CLI 与测试复现并断言了以下错误码：

| 注入 | 错误码 | 验证点 |
| --- | --- | --- |
| `schema_version=9.9` | `E_SCHEMA` | 指向版本字段路径 |
| 删除必填 `session_id` | `E_SCHEMA` | 指向 `$` 下缺失字段 |
| 非法 JSON | `E_DECODE` | 返回解析错误 |
| `MaxInt64 * 2` | `E_OVF` | 含字段和两个操作数 |
| 请求能力外缩放档 | `E_PARA` | 含屏 ID 与实际能力 |
| 区域完全在屏外 | `E_PARA` | 裁剪后为空 |
| 同 tick/revision 不同 payload | `E_CONFLICT` | 指出冲突事件 |
| 窗口待分发直接提交 | `E_STATE` | 原状态不变 |
| 第二窗时刻回退 | `E_ORDER` | 含倒置时刻操作数 |
| 当前 chroma 不在能力表 | `E_CAP` | 能力冲突 |
| 检查点内容哈希改写 | `E_DECODE` | 第 1 窗整体失效 |
| 算法版本改写 | `E_DECODE` | 版本不匹配拒绝 |
| 源版本改写 | `E_DECODE` | 版本不匹配拒绝 |
| 检查点文件写成坏 JSON | `E_DECODE` | 打开时拒绝 |

## 复现方式

```bash
go test -count=1 ./...
go run ./cmd/screenrelay verify
```

成功时 CLI 最后一行应为：

```text
report: artifacts/reports/verification-report.json
```

且进程退出码为 0。
