# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

```text
go version go1.22.12 linux/amd64
CGO_ENABLED=0
SQLite: modernc.org/sqlite v1.29.10（纯 Go）
```

当前执行容器是 WSL 文件环境，但没有 `cmd.exe` / `powershell.exe` 互操作入口，因此下列自动化结果在 Linux Go 工具链实跑；仓库已提供 CRLF 的 Windows `.bat` 和 PowerShell 脚本，需在 Windows 本机执行 `scripts\demo.bat` 做最终平台确认。

已额外执行 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...` 和 Windows 测试二进制交叉编译，均通过。

## 自动化测试

命令：

```bash
go vet ./...
go test ./...
```

结果：全部通过。详细日志：`reports/go-test-verbose.log`。

覆盖的关键用例：

- `TestGenerateLoadAndDecodeError`：固定种子生成、加载和损坏 PNG 的 `E_DECODE`。
- `TestLoadMetaMismatchNamesField`：尺寸/分块网格不符的 `E_META field=width`。
- `TestParameterValidationFieldNames`：`color_tolerance`、`gradient_threshold`、`structure_size` 的 `E_PARA`。
- `TestRegionCountAndQualityErrors`：`E_REGION` extra 区域和 `E_QUAL` 三项偏差。
- `TestParallelAndSerialMasksArePixelIdentical`：4 worker 并行与单线程掩码逐像素一致，并观测到真实峰值并行度。
- `TestStateMachineIllegalTransitionKeepsState`：非法迁移返回 `E_STATE` 且状态不变。
- `TestBadSeedFailsOnlyOwningChunk`：坏种子返回 `E_SEED`，6 块中其他 5 块的 15 个阶段仍提交。
- `TestBudgetExhaustionRecordsBlockingQueue`：小分块 + 1 worker + queue=1 时记录 `queue_budget_exhausted`。
- `TestThreeWayResumeTamperAndIndependentVerify`：实时/恢复/全量一致；哈希篡改被独立验证器定位，恢复时整快照作废并重算。
- `TestPreviewDivergenceReport`：`E_DIVERGE`、三项实测值、首个不一致像素和最大差异像素。
- `TestParameterEditsKeepCommittedStageFrozen`：已提交阶段保留旧参数版本，未提交阶段使用新版本。

## 本地演示

命令：

```bash
./scripts/demo.sh
```

结果：退出码 `0`。原始输出保存在 `reports/demo-run.log`。

| 场景 | 三路径 | 独立验证 | 区域数 | 阶段数 | 预览倍率/耗时 | 预览一致性 |
| --- | --- | --- | ---: | ---: | --- | --- |
| `blocks` | PASS | PASS | 3/3 | 18/18 | 0.5× / 1ms | PASS |
| `low-contrast` | PASS | PASS | 3/3 | 18/18 | 0.5× / 1ms | PASS |
| `embedded-noise` | PASS | PASS | 4/4 | 36/36 | 0.5× / 1ms | PASS |

预览延迟上限均为 `250ms`，实测远低于上限。预览最大偏差：

| 场景 | 最大重心偏差(px) | 最大面积相对误差 | 像素不一致率 |
| --- | ---: | ---: | ---: |
| `blocks` | 0.009 | 0.0797 | 0.00370 |
| `low-contrast` | 0.353 | 0.0481 | 0.00482 |
| `embedded-noise` | 0.124 | 0.0510 | 0.00332 |

声明的预览容差为重心 `2.5px`、面积相对误差 `0.10`、像素不一致率 `0.04`，三个场景均通过。

## 预算排队实测

来自 `live` 路径的 `budget_metrics`：

| 场景 | worker 预算/峰值 | queue 预算 | 排队事件数 | 峰值线程像素缓冲 | 上限 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `blocks` | 3 / 3 | 2 | 6 | 20480 | 49152 |
| `low-contrast` | 3 / 3 | 2 | 6 | 32000 | 49152 |
| `embedded-noise` | 3 / 3 | 2 | 25 | 20480 | 49152 |

排队原因均为 `queue_budget_exhausted`，没有静默丢块。

## 三路径哈希

每次运行会重新生成 DB，完整哈希以 `reports/threeway-<scene>/three-way.json` 为准。最近一次记录的短哈希：

| 场景 | 拼接掩码 | 几何统计 | 阶段数 | 首个像素分歧 | 首个阶段分歧 |
| --- | --- | --- | ---: | --- | --- |
| `blocks` | `56726e5778d60f10` | `892bfebecfe78faa` | 18 | 无 | 无 |
| `low-contrast` | `b9dd2e8fcb7a305e` | `c1af4523743ca7fa` | 18 | 无 | 无 |
| `embedded-noise` | `8107ee26d73b928d` | `adaf241a0dd0463e` | 36 | 无 | 无 |

`live` 与 `resume` 均和 `full` 基线一致。

## Web 烟测

已实际启动 `maskpipe serve` 并验证：

- `GET /` 返回嵌入 HTML；
- `GET /api/state` 返回 18/18 提交阶段且独立验证为 OK；
- `GET /artifact?x=0&y=0&stage=clean` 返回合法 64×64 PNG；
- `GET /image/final` 返回合法全图掩码 PNG；
- `GET /api/preview?compare=1` 返回同参数版本且 `comparison.ok=true`；
- 非法重试坐标返回 HTTP 400 与 `E_STATE`。

## Windows 本机复验建议

```bat
scripts\check-env.bat
scripts\test.bat
scripts\demo.bat
scripts\serve-demo.bat
```

若希望显式确认命令可用，可在 Windows Command Prompt 中执行 `where go` 和 `go version`；本项目要求 `go1.22` 或更新版本。
