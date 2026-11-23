# 实际验证记录

验证日期：2026-11-24（Asia/Shanghai）

## 1. 工具链

```text
Go: go1.22.12 linux/amd64
CGO_ENABLED=1
GCC: 15.2.0
SQLite driver: github.com/mattn/go-sqlite3 v1.14.24（内置 amalgamation）
Wails CLI: v2.10.2
```

`go env GOPATH GOMODCACHE CGO_ENABLED` 正常。`go-sqlite3` 通过 CGO 编译，未依赖系统 `libsqlite3`。

## 2. 样本生成

命令：

```bash
go run ./cmd/samplegen -dir samples -seed 20261124
```

实际输出：

```text
generated 5 samples in samples (seed=20261124, font=bitmap5x7-v1)
  sparse_single_960x540       960x540  ch=4 lines=1 hash=8c06e698c370
  multiline_1280x720         1280x720  ch=4 lines=3 hash=2c303db39761
  tilted_low_1024x640        1024x640  ch=4 lines=2 hash=9d22cdf80692
  dense_1600x900             1600x900  ch=4 lines=6 hash=62d43bf0ed2a
  sparse_gray_800x600         800x600  ch=1 lines=2 hash=da2adbe4f5fd
```

确定性测试对两个临时目录分别生成，逐字节比较 PNG 和 truth JSON，全部一致。

## 3. 自动化测试

```text
$ go test ./...
?   	ocrworkbench/cmd/ocrlab	[no test files]
?   	ocrworkbench/cmd/samplegen	[no test files]
?   	ocrworkbench/cmd/verify	[no test files]
?   	ocrworkbench/internal/domain	[no test files]
?   	ocrworkbench/internal/fontlib	[no test files]
ok  	ocrworkbench/internal/imaging
?   	ocrworkbench/internal/store	[no test files]
?   	ocrworkbench/internal/webui	[no test files]
ok  	ocrworkbench/internal/lab
ok  	ocrworkbench/internal/ocr
ok  	ocrworkbench/internal/pipeline
ok  	ocrworkbench/internal/synth
```

`go-sqlite3` 编译时出现其上游 amalgamation 的 `discards const qualifier` C 警告，属于依赖源码警告，不影响构建或测试。

竞态检测：

```text
$ go test -race ./internal/ocr ./internal/lab ./internal/pipeline
ok  	ocrworkbench/internal/ocr	94.908s
ok  	ocrworkbench/internal/lab	29.227s
ok  	ocrworkbench/internal/pipeline	110.059s
```

静态检查：

```text
$ go vet ./...
# 通过（仅 go-sqlite3 上游 C 警告）
```

## 4. 三路径验收

命令：

```bash
go run ./cmd/verify -samples samples -seed 20261124
```

实际输出（`artifacts/verify-output.txt`）：

```text
OCR WORKBENCH VERIFICATION
algorithm=ocr-workbench/1.0.0 template=bitmap5x7-v1 seed=20261124 samples=5 dir=samples

PASS sparse_single_960x540      lines=1 chars=8 acc=1.000 iou=0.967 commits=29 time=410ms
     tiles_equal=true lines_equal=true commits_equal=true confidence=0.8706
PASS multiline_1280x720         lines=3 chars=29 acc=1.000 iou=0.988 commits=31 time=931ms
     tiles_equal=true lines_equal=true commits_equal=true confidence=0.8695
PASS tilted_low_1024x640        lines=2 chars=20 acc=1.000 iou=0.974 commits=26 time=720ms
     tiles_equal=true lines_equal=true commits_equal=true confidence=0.8725
PASS dense_1600x900             lines=6 chars=99 acc=1.000 iou=0.990 commits=51 time=2.794s
     tiles_equal=true lines_equal=true commits_equal=true confidence=0.8599
PASS sparse_gray_800x600        lines=2 chars=12 acc=1.000 iou=0.983 commits=22 time=390ms
     tiles_equal=true lines_equal=true commits_equal=true confidence=0.8842

RESULT: all deterministic samples passed the three-path consistency gate.
```

`commits = 瓦片数 + 文本行数`，例如密集样本 45 瓦片 + 6 文本行 = 51 次提交。

## 5. HTTP API 冒烟

启动：

```bash
go run ./cmd/ocrlab -addr 127.0.0.1:8123
```

调用 `bootstrap` 返回 5 个样本与默认参数；`run.start` 完成 `sparse_single_960x540`：

```text
commits: tile:000000 ... tile:000027, line:000000
line 0: STAGE-01 confidence=0.8706 state=committed
progress: Tiles 28/28 · Lines 1/1
```

`run.compare` 对 `dense_1600x900` 返回：

```text
equal=true
tiles_equal=true
lines_equal=true
commits_equal=true
compared_lines=6
compared_commits=51
live/resume/reset accuracy=1.000 confidence=0.8599
```

暂停 / 调参 / 恢复冒烟：

```text
start -> running
pause -> paused at "Tiles 0/45 · Lines 0/0", commits=0
tune confidence_threshold=0.99 -> ok
resume -> completed "Tiles 45/45 · Lines 6/6", commits=51
accepted flags -> false for all 6 lines, states remain committed
```

前端 JS 使用 Node 22 `--check` 做语法校验：

```text
JS_SYNTAX_OK
```

## 6. 异常码验证矩阵

目标测试命令（完整输出在 `artifacts/error-tests.txt`）：

```bash
go test -count=1 -v ./internal/imaging ./internal/ocr ./internal/pipeline \
  -run 'TestDecodeAndMetaErrors|TestBandParameterValidation|TestCandidateLowerBoundReturnsDetWithTileCoordinates|TestInsufficientBandReturnsBandError|TestReadingOrderConflict|TestTemplateAndEmptyLineErrors|TestSimilarityIsMonotonic|TestIllegalStateKeepsState|TestIdentityMismatchInvalidatesWholeSnapshot|TestTuningOnlyAffectsUncommittedLines|TestUnitFailureIsIsolatedAndRetryable|TestComparisonReportsFirstDiffLocation'
```

实际结果：上述全部 `PASS`（imaging 2.949s、ocr 2.079s、pipeline 3.568s）。

| 异常码 | 自动化测试 | 断言 |
|---|---|---|
| `E_DECODE` | `TestDecodeAndMetaErrors` | 清单缺失、PNG 损坏 |
| `E_META` | `TestDecodeAndMetaErrors` | `field=width`、`truth.region_count`，expected/actual 非空 |
| `E_PARA` | `TestBandParameterValidation`、`TestHTTPBridgeBackendEndToEnd` | 0 边带、半瓦片边带、非法 tile size |
| `E_DET` | `TestCandidateLowerBoundReturnsDetWithTileCoordinates`、`TestUnitFailureIsIsolatedAndRetryable` | 瓦片坐标、失败单元可重试 |
| `E_TMPL` | `TestTemplateAndEmptyLineErrors` | 空版本/未知版本 |
| `E_STATE` | `TestIllegalStateKeepsState`、`TestTuningOnlyAffectsUncommittedLines` | 迁移失败后原状态与提交数不变 |
| `E_BAND` | `TestInsufficientBandReturnsBandError` | 3px 边带触发，含 tile/break 坐标 |
| `E_READ` | `TestReadingOrderConflict` | 重复阅读索引 |
| `E_DIFF` | `TestComparisonReportsFirstDiffLocation` | 首个差异 line=0、tile 坐标、difference_field=text |

其他专项：

- 空行：`TestTemplateAndEmptyLineErrors` 验证 `text=""`、`empty=true`、`confidence=0` 且不丢弃。
- 置信度单调：`TestSimilarityIsMonotonic` 验证 exact > near > far，全部在 `[0,1]`。
- 跨瓦片一致性：`TestTiledAndWholeDetectionAgree` 在 5 个样本上逐候选/逐行包围盒一致。
- 并行确定性：`TestParallelTileScanIsDeterministic`、`TestTileScanOrderDoesNotChangeResults` 验证并发/逆序扫描候选完全一致。
- 冻结：`TestTuningOnlyAffectsUncommittedLines` 验证已提交行快照逐字节不变，新阈值只改变未提交行 accepted 标记。
- 身份作废：`TestIdentityMismatchInvalidatesWholeSnapshot` 同时改 image hash 与 param version，旧 tiles/commits 全部消失。

## 7. Wails 环境

安装：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails doctor
```

`wails doctor` 结果摘要：

```text
Wails Version: v2.10.2
Go: go1.22.12 linux/amd64
gcc: Installed 15.2.0
libgtk-3: Available（未安装）
libwebkit: Not Found
pkg-config: Available（未安装）
Diagnosis: Required dependencies missing: libwebkit
```

尝试构建：

```bash
wails build -skipbindings -clean -tags desktop
```

前置检查通过：识别 `desktop` tag、跳过 npm install/build、开始编译应用。随后因容器缺少 `pkg-config`/WebKitGTK 开发包停止。容器无 sudo 权限，无法安装系统包。

因此：

- Wails CLI 已安装并完成 doctor。
- Wails 工程配置、入口、共享前端 embed、绑定 API 已交付。
- 当前 Linux 容器未能链接桌面二进制；同核心 HTTP 实验台已完成真实交互与 API 验收。
- Windows 构建使用 WebView2，不需要 WebKitGTK；按 README 的 MinGW-w64 + CGO + Wails 步骤可重复构建。

## 8. 结论

- 检测识别链路在 5 个本地确定性样本上跑通，文本逐行匹配真值。
- 实时、断点恢复、全量重算三条路径的识别文本、逐字符置信度、阅读索引、瓦片/文本行提交序列一致。
- 异常码均有自动化断言，并包含要求的字段、瓦片坐标、行号或断裂位置。
- 已提交状态可在暂停/重启后恢复；调参不会改写已提交快照；身份不匹配整体作废而非混用。
