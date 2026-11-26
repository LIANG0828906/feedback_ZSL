# 实际验证记录

验证环境：WSL Ubuntu，`go version go1.22.12 linux/amd64`，仅使用 Go 标准库与本仓库生成样本。

## 1. 自动化测试

命令：

```bash
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

实际结果：

- `go vet ./...`：退出码 `0`。
- `go test ./... -count=1`：全部包 `ok`，退出码 `0`。
- `go test -race ./... -count=1`：全部包 `ok`，`race_exit=0`。

覆盖的关键测试：

- `internal/conv`
  - `TestScaleExposureGainIntegerRoundTrip`
  - `TestOverflowCodedError`：核对 `E_OVF`、字段 `exposure.stack[].value_ms` 与两个操作数。
  - `TestGainParameterRange`：核对非法增益返回 `E_PARA`、字段名和合法范围。
- `internal/projectio`
  - `TestThreeSchemaRoundTripAndTraces`
  - `TestDecodeUnknownVersionAndMissingField`：核对未知版本与 `$.seed`、`$.geometry.width` 路径的 `E_DECODE`。
  - `TestV1LossReportStableAfterImportExport`：核对 `E_LOSSY` 清单及再次导入/导出后的条目和 digest 一致。
- `internal/checkpoint`
  - `TestRecoverRejectsContentHashMismatch`
  - `TestRecoverRejectsVersionMismatch`：均核对整体拒绝恢复并返回 `E_VER`。
- `internal/graph`
  - `TestGainEditInvalidatesOnlyActiveDependencies`
  - `TestEquivalentVersionSwitchDoesNotInvalidateBlocks`
- `internal/pipeline`
  - `TestRunVerification`：生成器真值、三档像素一致、增量/恢复/全量报告一致、未受影响块 `3,2` 持久化复用、dense LUT 真实命中、medium 边界核对。
  - `TestAdaptiveThresholdInjection`
  - `TestSchedulerPixelBufferParameter`
  - `TestInvalidGainReturnsEParam`
  - `TestDriftBeyondBoundReturnsEDrift`：核对块、波段、实测偏差和误差界。
  - `TestCompareReportsReturnsEDiffLocation`：核对首个不一致定位到块和 `B01`。
- `cmd/msdemo`
  - `TestProcessKillAndResume`：真实构建 CLI，子进程 SIGKILL 后重启恢复并与全量报告比对。

## 2. 本地演示产物

命令：

```bash
go run ./cmd/msdemo all --out artifacts/demo
```

实际输出：

```text
OK verification=PASS blocks=12 bands=5 output=artifacts/demo
workbench=artifacts/demo/workbench/index.html
```

重复执行同一命令仍为 `PASS`；验证编排会重置自己拥有的检查点文件。

产物：

- 三版本工程：`artifacts/demo/samples/project.v1.json`、`project.v2.json`、`project.v3.json`
- 确定性样本/真值/三档摘要：`artifacts/demo/samples/sample-set.json`
- 检查点：`artifacts/demo/checkpoints/*.db`
- SQLite DDL：`artifacts/demo/checkpoints/block_stage.sql`
- 诊断界面：`artifacts/demo/workbench/index.html`
- 完整机器可读验证结果：`artifacts/demo/workbench/verification.json`

最终一次摘要中的实测计数：

- 块数/波段数：`12 / 5`，图像 `16x12`，块 `4x4`。
- 三版本规范哈希一致：`820e6dc04b08bfcc...`。
- 装载痕迹：v1=`21` 条，v2=`6` 条，v3=`10` 条映射痕迹。
- v1 降级清单：`10` 项（5 个嵌套树路径 + 5 个增益字段），digest `5f29443d8658637ae75793abcd4a5740`。
- B05 增益编辑：失效 `22` 个活动节点（11 个块波段节点 + 11 个重建节点），复用清单 `50` 项；全零块 `3,2` 的重建结果未失效。
- sparse→dense：失效 `72` 个节点（60 个块波段节点 + 12 个重建节点），原因含观测数 `80` 与阈值 `20/40`。
- 等价版本切换：仅版本来源节点失效，`12` 个块重建结果全部复用。
- medium 边界核对示例：row/recomputed radiance 均为 `176084`，remainder 均为 `16048000000`，`match=true`。
- dense LUT：每个块均有真实命中和跨桶回退；默认步长 `4`，波段量化误差界为 `1`，重建通道误差界示例为 `[10,10,10]`。

## 3. 强制 SIGKILL 与恢复

命令：

```bash
go build -o /tmp/msdemo-verify ./cmd/msdemo
state=$(mktemp -d /tmp/ms-verify-XXXXXX)
/tmp/msdemo-verify crash --state "$state" --after 5
/tmp/msdemo-verify resume --state "$state"
/tmp/msdemo-verify full-run --state "$state"
/tmp/msdemo-verify compare-reports \
  -want "$state/full-report.json" \
  -got "$state/resume-report.json"
```

实际结果：

```text
Killed ... /tmp/msdemo-verify crash --state ... --after 5
crash_exit=137
OK resumed blocks=12 records=24
OK full blocks=12 records=24
OK reports match block pixels, reconstruction, method and commit sequence
compare_exit=0
```

崩溃点为第 `5` 个阶段提交后，即 `0,0/quant`、`0,0/recon`、`1,0/quant`、`1,0/recon`、`2,0/quant` 已提交；恢复报告仍包含稳定的 `24` 条阶段记录（12 块 × quant/recon），末块为 `3,2/recon`。

## 4. 诊断服务

命令：

```bash
go run ./cmd/msdemo serve --dir artifacts/demo --addr 127.0.0.1:8088
```

实测：

- `curl -fsSL http://127.0.0.1:18090/workbench/index.html`：退出码 `0`，`9863` 字节。
- `curl -fsS http://127.0.0.1:18090/workbench/report-data.js`：退出码 `0`，约 `48KB`。

Go 文件服务会把 `/workbench/index.html` 规范重定向到 `/workbench/`，浏览器可直接跟随。

## 5. 业务码核对

| 业务码 | 验证位置 |
| --- | --- |
| `E_DECODE` | 未知 schema 版本、缺少 `$.seed` 或 `$.geometry.width` |
| `E_LOSSY` | 导出 v1 时的嵌套光谱带与增益修正清单 |
| `E_OVF` | 毫秒曝光乘 `1000` 溢出，错误含字段与两个操作数 |
| `E_PARA` | 增益超出 `500000..2000000 ppm`；调度缓冲小于块需求 |
| `E_DRIFT` | 注入 `+500` 量化偏差后超过重建通道误差界 |
| `E_VER` | 篡改检查点 `content_hash` 或算法/参数版本 |
| `E_DIFF` | 报告比对定位首个块/波段不一致 |
