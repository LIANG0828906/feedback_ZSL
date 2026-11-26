# 实现说明与需求追溯

模块名：`lightprobe`，Go 1.22，仅使用标准库。入口库位于仓库根包，命令行/工作台位于 `cmd/probebench`。

## 快速运行

```bash
go test ./...
go test -race ./...
./scripts/verify.sh

go run ./cmd/probebench verify --dir .probebench/acceptance
go run ./cmd/probebench serve --addr 127.0.0.1:8088 --dir .probebench/workbench

go build -o /tmp/probebench ./cmd/probebench
/tmp/probebench crash --dir .probebench/manual   # 提交 room-a 后 SIGKILL，shell 可见 137
/tmp/probebench resume --dir .probebench/manual
```

## 领域链路

1. `generate.go` 使用固定种子 `DefaultSeed` 确定性生成三间相邻房间、`5×3×3` 探针格点、解析三线性场、可见性计数、9 个二阶球谐系数、12 条重采样指令、6 个相机帧、137 个查询和 128 个屏幕像素。
2. `sample_codec.go` 给样本封包加入 SHA-256 校验；样本损坏返回 `E_DECODE`。
3. `validate.go` 校验包围盒、格点归属/重合、定点 SH 范围和指令时间单调性，错误均为 `E_SCHEMA` 并带对象 ID 与字段名。
4. `reconstruct.go` 使用 `math/big` 做定点乘积与累加，只在 SH 聚合和样本加权末端除法截断；每探针每通道写两条 `TruncationRecord`。整数加法天然不受项顺序影响。
5. `interpolate.go` 对八角点做三线性插值，权重按 `WeightScale` 地板截断后用整数权和归一化；边界查询镜像到邻房并按声明的线性跨域比例混合。
6. `report.go` 从已观测的 SH 刻度、样本数 `N`、归一化权重、截断误差和混合比例推导四项 radiance 上界；真值只由 `ExpectedRadiance`/`EvalField` 解析计算。
7. `parallel.go` 实现有界 worker pool：`Workers` 限制并行度，`BufferLimit` 限制每 worker 本地探针缓冲，`QueueBudget` 限制待处理任务通道；预算满时阻塞入队并记录原因和纳秒等待。
8. `depgraph.go` 建立 `radiance → sh/reconstruction → blend → query → pixel` DAG。缓存键为自内容、房间、分辨率版本、参数版本和排序后的上游键集合的 SHA-256。
9. `state.go` 实现 `pending / collecting / reconstructed / committed / failed` 状态迁移；非法迁移返回 `E_STATE` 且保留状态和进度。
10. `checkpoint.go` 按环境条款采用标准库文件持久化：全局事务锁、临时文件、`fsync`、原子 `rename` 和目录 fsync。每房单事务写入探针摘要、radiance buffer、依赖集合、内容哈希和三类版本。哈希/版本不符在加载时判整房 stale 并重建；结构损坏返回 `E_DECODE`。
11. `pipeline.go` 串联房间隔离、并行重建、查询、误差报告、像素着色、依赖图和提交顺序；`DiffPipelines` 按房间、IJK、字段顺序定位首个 `E_DIFF`。
12. `workbench.go` 与 `static/index.html` 通过 `embed` 提供无外部依赖的本地诊断工作台。

## 错误码

| 码 | 触发面 |
| --- | --- |
| `E_SCHEMA` | 场景装载校验失败，带对象 ID/字段 |
| `E_VAL` | 参数非法、格点越界、通道/进度越界 |
| `E_DECODE` | 样本封包或检查点文件损坏 |
| `E_STATE` | 房间状态机非法迁移 |
| `E_DIFF` | 三条路径逐格点/像素/不确定标记/提交序列不一致 |
| `E_CANCEL` | 取消导致的管线边界错误包装 |

## 误差上界口径

默认 `FixedScale=WeightScale=BlendScale=1e6`、`SampleLambda=8`。每个角点使用：

- SH：`48/(2S) + 1/S`，覆盖系数量化和定点聚合截断。
- 计数取整：`0.5·λ / ((N+λ-0.5)^2) + 1/S`，来自四舍五入误差不超过 0.5 以及样本权重对 `N` 的差分敏感度。
- 权重：若定点权和为 `s=1-δ`，归一化权重相对精确权重的 L1 上界为 `2δ/s`，另加一次定点除法量子 `1/S`。
- 跨域混合：`< 1/BlendScale + 1/S`。

查询上界按主/邻域实际混合权重和八角点归一化权重累积。测试强制每个真值落在估计值的上下界内，并强制实测偏差不超过累积上界。

## 验证索引

完整原始输出见 `../artifacts/verification-run.log`，摘要见 `VERIFICATION.md`。
