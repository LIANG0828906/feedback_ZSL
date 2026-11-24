# 实际验证记录

验证日期：2026-11-24

执行环境：

- Go：`go1.22.12 linux/amd64`
- 本地模块缓存：仓库内 `.gomodcache`
- Windows 目标检查：`GOOS=windows GOARCH=amd64 go build ./...` 通过
- 外部依赖：无

## 命令

```bash
go test ./...
go test -race ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build ./...
go run ./cmd/audioeval demo --dir artifacts/demo
go run ./cmd/audioeval verify \
  --sample-dir artifacts/demo/samples/q3 \
  --run-dir artifacts/demo/runs/resume
```

## 演示结果

最新一次演示输出：

```text
demo complete: artifacts/demo
three paths match; verifier blocks=32 frames=8; queue waits=8; PSNR monotonic Q1<Q3<Q5
```

三路径：

- `realtime_vs_full = match`
- `resume_vs_full = match`
- 取消点持久化：3 个块
- 恢复加载：3 个块
- 总块数：32

独立验证：

```json
{
  "ok": true,
  "checked_blocks": 32,
  "checked_frames": 8
}
```

预算控制：

- 最大工作线程：4
- 队列预算：1
- 实际记录到的排队任务：8（数量随线程完成时序变化，但每次耗尽都必须记录）
- 每个事件均记录 `queue_budget_full`、块坐标和等待毫秒数
- 小任务等待可能不足 1 ms，因此整数毫秒字段可以为 0，但阻塞事件仍然存在
- 无块丢失，提交序列仍为 `0..31`

质量档位单调性（逐帧平均 PSNR，整数微 dB）：

| 档位 | 平均 PSNR |
|---|---:|
| Q1 | 82,646,491 |
| Q3 | 92,946,273 |
| Q5 | 109,173,004 |

主样本逐帧真值对照示例：

| 帧 | PSNR | 真值期望 PSNR | 量化残留标准差 Q16 | 真值噪声基底 Q16 | 上界 |
|---:|---:|---:|---:|---:|---:|
| 0 | 92,065,292 | 92,078,136 | 53,484 | 53,458 | 18 |
| 1 | 93,181,013 | 92,078,136 | 47,503 | 53,458 | 18 |

## 自动化测试覆盖

- 固定种子生成完全一致，原始与重建序列对齐。
- 缺失清单字段、缺失真值字段、非法窗口和预算返回 `E_PARA`。
- PCM 字节损坏返回 `E_DECODE` 并定位帧哈希字段。
- 非法状态迁移返回 `E_STATE` 且保持原状态；提交结果冻结。
- 边带不足返回 `E_BAND`，包含块坐标和最小边带。
- 块合并指标与整帧单次评估逐字段一致。
- 队列预算耗尽时阻塞并记录原因，所有任务仍按序提交。
- 取消后不发布完整报告；恢复结果与全量重算一致。
- 快照参数版本失配时整体作废并重算。
- 独立验证器能定位被篡改的首个帧指标。
- Q1 < Q3 < Q5 的 PSNR 单调性。
- `go test -race ./...` 通过。

完整机器可读结果见 `artifacts/demo/validation.json`。
