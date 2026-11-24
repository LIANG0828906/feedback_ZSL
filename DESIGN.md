# 声学测量与校准组件 — 技术说明与验收手册

面向游戏与互动媒体音频设备的声学测量、补偿曲线拟合、固件配置导出与回环一致性校验组件。
纯 Go 标准库实现，无第三方依赖、无容器，跨平台（文件路径一律 `filepath`，原子写走 `tmp + rename`）。

## 1. 目录结构

| 路径 | 职责 |
| --- | --- |
| `errors.go` | 稳定业务码 `E_META/E_CURVE/E_PARA/E_FIT/E_WIN/E_ECHO/E_STATE/E_DIFF/E_FW/E_MAP/E_SEG/E_STORE` |
| `sample.go` | 固定种子 LCG 确定性样本：多通道 IR、位置元数据、目标曲线、解析真值 |
| `validate.go` | 测量记录结构校验与 `FitConfig` 参数越界检查、JSON 清单原子读写 |
| `fit.go` | DFT 频响测量、对数频三角基岭回归、FIR 合成、残差上界校验 |
| `firmware.go` | 分段规划、时序窗约束、窗内合并诊断、确定性大端二进制编码 |
| `exporter.go` | 四态状态机、提交冻结边界、串行原子检查点存储、快照作废 |
| `paths.go` | 交互式 / 断点恢复 / 一次性全量三条导出路径与 `E_DIFF` 字节比对 |
| `echo.go` | 回显频响按网格对齐比对、`E_ECHO` 首个超差点定位 |
| `pool.go` | 有界并行 worker（最大并行度 + 每线程频响缓冲上限） |
| `panel.go` | 纯文本可视化面板（状态、残差/偏差曲线、字节摘要、回显明细） |
| `device/` | 回环 TCP 模拟设备服务、固件解析、四类异常注入、串行客户端 |
| `cmd/samplegen` | 确定性样本生成器 |
| `cmd/device` | 独立模拟设备服务（监听 127.0.0.1 高位端口） |
| `cmd/demo` | 端到端验收演示（三路径一致 + 回显 + 异常链路 + 面板） |

## 2. 关键设计

### 2.1 确定性
- 自实现固定参数 LCG（`sample.go`），不依赖全局 `math/rand` 源，跨机器/跨 Go 版本逐样本一致。
- 拟合为纯函数：相同 `Sample + FitConfig` 必产出相同 `CalibrationModel`。
- 固件编码（`FirmwareConfig.Encode`）只使用大端序定长字段、按逻辑通道号排序的映射、长度前缀数组；
  不含 map 遍历、goroutine ID、时间戳或指针，重复编码逐字节一致。

### 2.2 拟合与残差
- 频响由单 bin DFT 测得幅度 dB；补偿目标 `y = 目标曲线 - 测量频响`。
- 对数频三角基（默认 6 基函数）岭回归 `(A'A+λI)β=A'y`，部分主元高斯消元求解。
- FIR 由预测增益经 IDFT + 因果移位合成；设备端用同一 `FIRResponseDB` 算法复算实际频响。
- 逐通道逐频点残差与样本内解析真值上界比较，超界返回 `E_FIT`（含通道/频点索引、实测值、上界）。
- λ 收紧时岭回归 SSE 单调不降、基函数数量（滤波器阶数）不变，由 `TestRegularizationMonotonicAndStableOrder` 守护。

### 2.3 分段、时序窗与合并
- 自然分段按通道 FIR 抽头与 `MaxCoeffsPerSeg` 切分；每段带事件起止时间（μs）、增益包络、内容哈希。
- 所有事件必须落在 `WindowUS` 声明窗内；非法窗/段数直接 `E_WIN`，详情含实测段数与窗宽。
- 自然段超过 `MaxSegments` 时只在同一通道内合并相邻段（杜绝跨通道混合 FIR），
  返回 `MergeDiag`（自然段数、上限、合并对）而非静默截断；不可满足时 `E_WIN`。

### 2.4 状态机与检查点
- 段状态 `pending -> exporting -> committed / failed`，合法迁移表驱动；
  非法迁移返回 `E_STATE` 且保持原状态；`committed` 为冻结终态。
- 每提交一段，在互斥锁内把系数、包络、内容哈希、目标指纹、参数版本、算法版本
  经 `tmp+rename` 原子写入 `data/checkpoints/.../segment-N.json`，写入串行、段序稳定。
- 重启加载时任一哈希/目标指纹/参数版本/算法版本不匹配，或段号不连续，
  整体快照目录作废并全量重算，禁止部分混用旧段。
- 强制终止后只能恢复已持久化段；未全部提交时 `finalize` 拒绝生成完整报告（`E_STATE`）。

### 2.5 三路径一致与回显
- 三条导出路径共用同一状态机、同一段规划与同一编码器，输出固件字节、提交序列、残差完全一致。
- `CompareResults` 逐字节比对，首个不一致处返回 `E_DIFF`（偏移、两侧字节、段号）。
- 设备服务按 `ACALFW01` 格式解析并重组每通道 FIR，回显实际施加频响；
  客户端按网格对齐比对，超容差点返回 `E_ECHO`（频点、本地值、回显值、偏差、容差）。

### 2.6 异常注入与并发边界
- 服务端支持 `firmware_version_reject / timing_window_violation / channel_map_conflict /
  segment_missing / echo_drift`，分别映射到 `E_FW/E_WIN/E_MAP/E_SEG/E_ECHO`，带段号或通道标识。
- 拟合编码可经 `Pool`/`ParallelFit` 并行，受最大并行度与单任务缓冲上限约束；
  检查点写入（`CheckpointStore` 锁）与设备下发（`Client` 锁）均串行，保证段序稳定。

## 3. 运行方式

```bash
go version                 # 需要 Go 1.22+
go build ./...
go test ./...              # 单元 + 回环集成测试
go test -race ./...        # 竞态检测

go run ./cmd/samplegen -dir data -n 3
go run ./cmd/device -port 39177            # 独立设备服务，127.0.0.1
go run ./cmd/demo                           # 端到端验收演示
```

## 4. 业务码对照表

| 触发条件 | 业务码 | 定位字段 |
| --- | --- | --- |
| 采样率/声道布局缺失、IR 长度不符 | `E_META` | 字段名 |
| 目标频点非单调/增益非有限 | `E_CURVE` | `target[i]` |
| 配置参数越界 | `E_PARA` | 字段名 |
| 残差超解析上界 | `E_FIT` | 通道 + 频点索引 |
| 时间窗/分段非法或越界 | `E_WIN` | 实测段数 + 窗宽 |
| 回显偏差超容差 | `E_ECHO` | 首个超差频点 + 两侧值 |
| 非法状态迁移 | `E_STATE` | 段号 + from/to |
| 三路径字节不一致 | `E_DIFF` | 字节偏移 + 段号 + 两侧字节 |
| 固件版本拒收/魔数非法 | `E_FW` | version/magic |
| 通道映射冲突 | `E_MAP` | 通道标识 |
| 段缺失/编号不连续 | `E_SEG` | 段号 |
| 检查点损坏/版本不匹配 | `E_STORE` | checkpoint |
