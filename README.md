# mcapture — 多通道采集联调桌面应用

面向多人语音与录制场景的多通道采集、漂移估计、分数延迟对齐与重采样工具。
全部样本由本地确定性生成器（固定种子）产出，无需真实设备与外部网络；
桌面诊断页面由 Go 本地 HTTP 服务 + 内嵌 HTML 提供，仅监听回环地址。

## 环境要求

- Go 1.22+（在 Go 1.22.12 验证；依赖锁定为兼容 1.22 的 `modernc.org/sqlite v1.34.5`，纯 Go、无 cgo）
- 不使用 Docker 或容器
- 一个可写工作目录（SQLite 检查点、导出结果）。全量/实时模式运行前会自动清空同名旧检查点

## 快速开始

```bash
go mod tidy
go build ./...

# 1) 生成确定性会话（固定种子；也可用 -shape chirp 切换扫频标记）
mcapture generate -dir ./work/sess -channels 4

# 2) 一次性全量重算（多线程，受预算控制）
mcapture run -dir ./work/sess -work ./work/full -workers 3

# 3) 实时采集路径（分段流式语义，单线程）
mcapture realtime -dir ./work/sess -work ./work/rt -workers 1

# 4) 断点恢复（复用版本与哈希匹配的已提交分段，其余重算）
mcapture resume -dir ./work/sess -work ./work/rs -workers 3

# 5) 三路逐样本一致性 + 异常码 + 提交序列自动比对
mcapture verify -dir ./work/sess -work ./work/verify -workers 3

# 6) 多声道导出（每通道 float64le + export.json 清单）
mcapture export -dir ./work/sess -work ./work/full -out ./work/out

# 7) 故障注入（分别走 E_CHAN / E_MARK / E_CLIP 链路）
mcapture inject -dir ./work/fault -fault chan
mcapture inject -dir ./work/fault -fault mark -marker 3
mcapture inject -dir ./work/fault -fault clip -gain 4

# 8) 诊断页面（浏览器打开，仅回环）
mcapture web -dir ./work/sess -work ./work/web -addr 127.0.0.1:8080
```

Windows PowerShell 下参数完全一致，仅路径分隔符不同。

## 目录结构

```
cmd/mcapture            CLI 入口（generate/run/realtime/resume/verify/export/inject/web）
internal/session        清单类型、确定性生成器、装载校验、错误码、裸数据 IO
internal/dsp            标记模板、匹配检测、加窗 sinc 插值与核表、最小二乘
internal/pipeline       漂移估计、对齐重采样、状态机、调度预算、检查点恢复、三路比对
internal/store          SQLite 检查点（单写入者串行原子、版本/哈希快照）
internal/web            回环 HTTP 服务与内嵌诊断页面（go:embed）
docs/                   技术说明与验证记录
```

## 错误码

| 码 | 触发条件 | 附带定位 |
|----|----------|----------|
| `E_META` | 清单字段缺失、点数不符、采样率不一致、通道标识冲突 | 通道 + 字段名 |
| `E_DECODE` | 裸数据长度/编码非法、出现非有限采样 | 通道 + 采样位置 |
| `E_DRIFT` | 标记不足或估计退化 | 通道 + 所用标记数 |
| `E_PARA` | 漂移率/插值阶数/窗长等参数越界，或插值越界 | 字段名 |
| `E_RESID` | 对齐后残余偏移超声明上界 | 通道 + 标记序号 |
| `E_STATE` | 分段状态机非法迁移（含改写已提交段） | 通道 + 分段 |
| `E_DIVERGE` | 多路径结果首个不一致采样 | 通道 + 分段 + 采样 |
| `E_CHAN` / `E_MARK` / `E_CLIP` | 通道缺失 / 标记缺失 / 增益溢出 | 通道、标记或采样 |

## 测试

```bash
go test ./... -count=1
```

覆盖：装载与校验错误码、漂移估计精度与随标记数收敛、分数插值、状态机、
参数越界、三路逐样本一致、检查点恢复复用、版本/哈希整体作废、预算阻塞事件、
三类故障注入链路与提交序列稳定性。详细设计与实测结果见 `docs/DESIGN.md`。
