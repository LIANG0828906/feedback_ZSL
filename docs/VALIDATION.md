# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

- WSL2 Ubuntu 26.04，Linux x86_64。
- Linux Go：`go version go1.22.12 linux/amd64`。
- Windows 原生 Go：便携版 `go1.22.12.windows-amd64`，
  输出 `go version go1.22.12 windows/amd64`。
- SQLite：`modernc.org/sqlite v1.29.10`，纯 Go，`CGO_ENABLED=0`。

## 一键脚本

```bash
bash scripts/validate.sh
```

脚本退出码为 0，末行：

```text
VALIDATE_OK
```

## Go 测试与静态检查

实际结果：

```text
$ go test ./... -count=1
ok      fixedsampler/internal/engine    0.002s
ok      fixedsampler/tests              0.161s

$ go vet ./...
（无输出，退出码 0）
```

测试覆盖：

- 三组生成样本全部通过独立验证器；
- 不同 worker/buffer 下逐分块、逐级结果一致；
- 末级位深编辑只失效第 3 级；
- 中间倍率编辑精确传播到后续级；
- 交换两级顺序后增量与空缓存全量一致；
- 对齐分块的依赖边不向无关邻块扩散；
- 取消后仅复用已持久化分块；
- 版本篡改触发 `E_CHECKPOINT`/`ErrSnapshotInvalid` 并整体作废；
- 验证器能定位首个 `E_DIFF` 分块、级和字段；
- 容差收紧时接受点数单调不增；
- 有界 worker 队列满时记录背压事件并阻塞。

说明：本环境的 `go test -race` 未能运行，因为 `gcc` 包装器指向的 `gcc-15` 文件缺失；
这是工具链环境问题。共享状态仍由 mutex、单连接 SQLite 和阶段内只读平面约束保护。

## Windows 构建

Linux 交叉编译：

```text
$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
WINDOWS_BUILD_OK
```

生成的 `sample-pipeline.exe`、`verify-result.exe`、`gen-samples.exe` 经 `file` 识别为
`PE32+ executable ... x86-64`，并通过 WSL interop 输出帮助。

在 Windows NTFS 目录使用原生 PowerShell 和便携 Go 1.22.12：

```text
NATIVE_WINDOWS_GO_BUILD_OK
```

原生 Windows 测试：

```text
ok      fixedsampler/internal/engine    0.287s
ok      fixedsampler/tests              1.180s
```

Windows EXE + NTFS SQLite 的样本验证：

```text
VERIFY_OK project=blocks420 chunks=24 points=5 truth_fp=95e278f1ac02f023448cdbc6e92a8c3c4c1f7344624e87364635163d8fe5e672
```

注意：把 Windows EXE 放在 WSL ext4/UNC 路径（`\\wsl.localhost\...`）上使用 WAL SQLite
曾出现 `database is locked`；复制到 `C:`/NTFS 后正常。这是 WSL/UNC 文件锁边界，不是
SQLite 事务逻辑失败。

## 本地样本精度

使用分块尺寸 8 的实际验证输出：

| 工程 | 分块数 | 已知点 | 最大定点偏差 | 声明容差 | 结果 |
| --- | ---: | ---: | ---: | ---: | --- |
| `blocks420` | 24 | 5 | `10312` | `100000` 或 `250000` | `VERIFY_OK` |
| `rampcolor444` | 15 | 9 | `0` | `100000` | `VERIFY_OK` |
| `mixed422` | 32 | 4 | `0` | `125000` | `VERIFY_OK` |

blocks 的非零最大偏差来自第 1 级已知点；最终量化色块点偏差为 0。所有点均在真值容差内。

三个工程的真值指纹：

```text
blocks420      95e278f1ac02f023448cdbc6e92a8c3c4c1f7344624e87364635163d8fe5e672
rampcolor444   ba76412470a62d73f31c400fe581cdd42f0d660b65747010cabaad158e56e486
mixed422       338124ba672bb14fe1019114a1f06f0dbed9f83a52ccb165e26c87d2f3656ab9
```

## 增量与全量一致

将 blocks 第 3 级从 8bit 改为 10bit：

```text
invalidated_nodes = 24
COMPARE_OK project=blocks420 chunks=24 sequences and all level fields match
```

即 24 个分块的第 3 级失效，第 1、2 级复用；增量库与 `--fresh` 全量库的提交顺序、
各级平面、分块坐标和全部 JSON 字段逐项相同。

将 blocks 第 2 级倍率从 2 改为 4 时，手动验证得到：

```text
invalidated_nodes = 48   # 24 个第 2 级 + 24 个下游第 3 级
COMPARE_OK project=blocks420 chunks=24 sequences and all level fields match
```

交换 mixed422 前两级顺序的自动化测试也通过增量/全量全字段比较。

## 误差上界样例

blocks420 的 `report.json` 累积上界（定点单位）：

```json
[
  {"seq":1,"weight_norm_fixed":16381,"accum_trunc_fixed":1,"color_quant_fixed":0,"bit_depth_error_fixed":0},
  {"seq":2,"weight_norm_fixed":32762,"accum_trunc_fixed":2,"color_quant_fixed":0,"bit_depth_error_fixed":0},
  {"seq":3,"weight_norm_fixed":32762,"accum_trunc_fixed":2,"color_quant_fixed":0,"bit_depth_error_fixed":500000}
]
```

第 3 级位深压缩增益把前两类旧误差缩放到目标位深单位；`500000` 是 `value_scale=1000000`
下最近舍入的半码值保守上界。报告中每条 bound 均含 `dependencies` 观测值列表。

rampcolor444 的色彩转换残差统计示例：

```text
R: max_abs_milli=960/1000 fixed unit
G: max_abs_milli=998/1000 fixed unit
B: max_abs_milli=848/1000 fixed unit
cumulative color bound = 1 fixed unit
```

## 强制终止恢复

直接二进制使用第 2 个分块提交后退出的钩子：

```text
$ ./sample-pipeline ... --crash-after-commit 2
simulated hard termination after 2 durable chunk(s)
DIRECT_EXIT=137
```

一键脚本使用第 3 个分块，重启后实际摘要：

```text
chunks=24
invalidated_nodes=63   # (24-3)*3
VERIFY_OK project=blocks420 chunks=24 points=5 ...
```

这证明只有 3 个已事务提交且版本/哈希匹配的分块被复用，其余 63 个级节点重算。

## 资源边界

CLI 参数：

```text
--workers       最大 worker 并行度
--buffer        每个 worker 的采样任务 channel 容量
--chunk-size    全分辨率坐标分块边长
```

单元测试 `TestWorkerPoolBufferFullBlocksAndRecords` 确定性地占满一个单线程队列，验证第三个
任务记录 `per_worker_sampling_buffer_full_blocking_enqueue` 并阻塞，释放后继续，不丢任务。
SQLite 最大连接数为 1，配合 `Store.mu` 串行提交，提交顺序固定为分块编号升序。
