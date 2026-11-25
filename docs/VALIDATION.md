# 实际验证结果

验证时间：2026-11-25（Asia/Shanghai）。完整原始输出保存在：

- `artifacts/validation/acceptance-run.log`
- `artifacts/validation/go-test-verbose.log`
- `artifacts/validation/acceptance-result.json`
- `artifacts/validation/go-env.txt`

一键复现：

```bash
./scripts/acceptance.sh
```

## 环境

```text
go version go1.22.12 linux/amd64
go env GOVERSION GOOS GOARCH CGO_ENABLED = go1.22.12 linux amd64 1
gcc (Ubuntu 15.2.0-16ubuntu1) 15.2.0
```

当前环境最初的 `gcc` 包装器指向缺失的工具链目录；验证时已将 `gcc-15`、`cpp`、`binutils`、`libc6-dev` 等用户态 deb 包解压到该包装器声明的 sysroot，恢复了 CGO 编译。仓库同时保留 `CGO_ENABLED=0` 的纯 Go SQLite 驱动后备。

## Go 测试

`go test -v ./...` 全部通过：

- `cmd/meshdoctor`
  - `TestProcessRecoveryBoundaries/stop_after_committed_stage`
  - `TestProcessRecoveryBoundaries/sigkill_before_smooth_then_resume`
- `internal/pipeline`
  - `TestThreePathsByteIdentical/tetra|cube|octa`
  - `TestCanceledContextDoesNotAdvance`
  - `TestBoundedParallelMatchesSerial`
  - `TestStateMachineLegalAndFrozen`
  - `TestSmokeAllSamples/tetra|cube|octa`
  - `TestOverLargeWeldReportsDegenerateRing`
- `internal/sample`
  - `TestGeneratedSamplesMatchManifest`
  - `TestLoadValidationErrorCodes`
- `internal/store`
  - `TestHashCorruptionInvalidatesWholeChain`
  - `TestVersionMismatchInvalidatesChain`
- `internal/web`
  - `TestDiagnosticPageAndState`

## 六阶段与真值拓扑

`go run ./cmd/meshdoctor verify --parallel=true` 的实际结论为 `verification ok=true`。三个样本最终均闭合、无边界边、无非流形边，并恢复到解析真值：

| 样本 | 最终 V/F/E/χ | 边界边 | 非流形边 | 三路径 | 恢复命中 |
|---|---:|---:|---:|---|---:|
| `tetra` | 4 / 4 / 6 / 2 | 0 | 0 | 逐字节一致 | 2（两阶段取消点） |
| `cube` | 8 / 12 / 18 / 2 | 0 | 0 | 逐字节一致 | 2 |
| `octa` | 6 / 8 / 12 / 2 | 0 | 0 | 逐字节一致 | 2 |

SAT 检测器在输入样本中实测到 `cube=3`、`octa=2` 对不共享顶点的自相交三角形；`tetra=0`，其缺陷集中在重复顶点、翻转面、非流形重复面和孔。

最终网格哈希（完整值见 `acceptance-result.json`）：

- `tetra`: `dbb592a14a13c585…`
- `cube`: `543c19e0ff37f606…`
- `octa`: `c4f195e371d7e61f…`

## 错误码契约

自动边界检查 `11/11 PASS`：

```text
PASS E_IDX   index out of range identifies face
PASS E_META  face count manifest mismatch names field
PASS E_MERGE over-large weld tolerance reports merged pairs and ring
PASS E_HOLE  unclosed target ring reports ring number
PASS E_PARA  laplacian weight outside [0,1]
PASS E_TOPO  euler change reports before and after
PASS E_STATE committed stage cannot move back to running
PASS E_VER   mixed algorithm/parameter version names stage
PASS E_DIFF  byte comparison reports first difference
PASS E_POINT known point outside tolerance identifies point
PASS E_LOOP  broken loop reports ring coordinates
```

结构化详情示例：

```json
E_MERGE: {"first_degenerate_ring": 0, "merged_pairs": 2, "union_merges": 2}
E_POINT: {"point_id":"kp-boundary","distance":15.016657417681207,"tolerance":1e-9}
E_LOOP:  {"ring_id":"loop-boundary","closure_error":15.588457268119896,
          "ring_coordinates":[[9,9,9],[10,9,9],[9,10,9]]}
```

`E_DIFF` 样例定位到 `diagnostic` 阶段、`byte_offset=35`、`vertex_index=0`、`diff_type=vertex_bytes`，并在 `summary_a/summary_b` 中列出两侧字节数和哈希。

## 取消与 `kill -9` 恢复边界

验收脚本构建独立 CLI 二进制后执行：

1. `run --sample tetra --stop-after 2` 在 `diagnostic,weld` 提交后停止：
   - 首次 `"cache_hits": 0, "cache_misses": 2`；
   - `committed=["diagnostic","weld"]`。
2. 同 DB 重启续跑：
   - `"cache_hits": 2, "cache_misses": 4`；
   - 提交序列恢复为完整六阶段。
3. 启动 `cube --hang-before smooth`，等待持久化 `running` 状态和 ready 文件后执行 `kill -9 <pid>`。
4. 重启同一 DB：
   - 陈旧 `running(smooth)` 被归一化为 `failed` 后重试；
   - `"cache_hits": 4, "cache_misses": 2`；
   - 完整序列为 `diagnostic,weld,fill,orient,smooth,stats`。

这证明只有 `diagnostic/weld/fill/orient` 四个已原子提交阶段被恢复；`smooth` 在强杀时未提交，因此被整体重算。

## HTTP 诊断页

默认手动启动地址为：

```bash
go run ./cmd/meshdoctor serve --addr 127.0.0.1:18080
```

验收脚本为避免开发机端口冲突，会通过 Python 申请一个空闲回环端口并把实际地址写入 `acceptance-run.log`；最后一次完整运行使用 `http://127.0.0.1:44630/`。

并验证：

- `GET /` 返回包含 `MeshDoctor` 标题的本地 HTML；
- `GET /api/state` 返回 `ok=true`、`report.ok=true` 和 3 个样本；
- 页面不请求外部站点、账号或数据集。

## 跨平台构建

已实际执行：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o artifacts/bin/meshdoctor-windows-amd64.exe ./cmd/meshdoctor
CGO_ENABLED=1 go build -o artifacts/bin/meshdoctor-linux-amd64 ./cmd/meshdoctor
```

Windows 无 MinGW 时使用 `CGO_ENABLED=0` 的纯 Go SQLite 后备；有 gcc/MinGW 时默认使用 CGO 版 `mattn/go-sqlite3`。
