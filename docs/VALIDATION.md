# 实测验收记录

## 环境

- 日期：2026-11-24（Asia/Shanghai）
- OS：Linux amd64
- Go：`go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.34.5`（pure Go，CGO 非必需）
- 样本：`seed-42-384x256`，384×256 RGB，分块 64，预览上限 250 ms

## 自动化测试

```text
$ go test ./...
ok  maskpipeline/internal/checkpoint
ok  maskpipeline/internal/engine
ok  maskpipeline/internal/sample
ok  maskpipeline/internal/segment
ok  maskpipeline/internal/version

$ go test -race ./internal/engine ./internal/checkpoint ./internal/segment
ok  maskpipeline/internal/engine
ok  maskpipeline/internal/checkpoint
ok  maskpipeline/internal/segment

$ go vet ./...
(no output)
```

测试覆盖：

- 固定种子生成两次的文件级确定性
- 缺清单/坏图像/缺掩码 `E_DECODE`
- 宽高/通道元数据 `E_META` 与字段名
- 参数越界 `E_PARA`
- 背景种子 `E_SEED`
- 区域外溢 `E_SPILL`
- 种子顺序反转后的逐像素确定性
- 版本快照不可变与 `E_VER`
- 状态机合法/非法迁移和状态保持
- 输出哈希损坏后的整会话作废
- 预览/精确一致性
- 暂停、恢复、全量三路径逐像素一致
- 坏块隔离、失败重试与独立验证
- 每阶段缓冲上限 `E_BLOCK`
- 验证器检测持久化分块篡改并定位块/阶段/像素

## 完整验收命令

```bash
go run ./cmd/maskpipeline generate -out samples -seed 42
go run ./cmd/maskpipeline run \
  -samples samples \
  -db artifacts/checkpoints.db \
  -report artifacts/acceptance.json \
  -session-prefix acceptance
```

结果：`overall_ok = true`。

### 预览与精确一致性

| 项目 | 实测 |
|---|---|
| 预览延迟 | 34.722603 ms |
| 延迟上限 | 250 ms |
| 降采样倍率 | 1 |
| BFS 预算 | 8192 / 区域 |
| 初始迭代 | 7341, 7131, 2960 |
| 预算截断回退 | false |
| 区域数量差 | 0 |
| 最大重心偏差 | 0 px |
| 最大面积相对误差 | 0 |

精确与预览掩码哈希均为：

```text
34ea30a062bafd15a5be0b2f383bbc8824f2ba0eaa555b0f94ddab1593dc1197
```

统计哈希均为：

```text
c31f3fc83fe88f9ac13ba2eacaf48b55821013b2f31b4095eba078d8980aca86
```

### 三路径一致性

24 个块，每块 3 阶段，共 72 条提交记录。

| 路径 | 状态 | 新增提交 |
|---|---|---:|
| 实时增量 | completed | 72 |
| 暂停后恢复 | completed | 6 + 66 |
| 一次性全量 | completed | 72 |

检查项全部为 true：

- 掩码像素相等
- 掩码哈希相等
- 区域统计相等
- 统计哈希相等
- 阶段提交序列相等

### 独立验证

对 `acceptance-incremental`、`acceptance-resume`、`acceptance-full` 三个会话均验证成功：

- 真值指纹匹配
- 72 个规范阶段提交
- growth/cleanup 分块重组成功
- stats 局部统计与 cleanup 重算一致
- 最终掩码哈希与统计哈希一致

独立命令示例：

```bash
go run ./cmd/maskpipeline verify \
  -db artifacts/checkpoints.db \
  -sample samples/seed-42-384x256 \
  -session acceptance-incremental
```

输出 `truth_fingerprint_ok = true`、`commit_count = 72`，阶段序列从 `0/growth` 稳定到 `23/stats`。

### 异常注入

| 场景 | 期望 | 实测 | 结果 |
|---|---|---|---|
| 背景种子 | `E_SEED` | `E_SEED` | 通过 |
| 最大容差导致外溢 | `E_SPILL` | `E_SPILL` | 通过 |
| 偶数结构元素 | `E_PARA` | `E_PARA` | 通过 |
| 跨版本提交 | `E_VER` | `E_VER` | 通过 |
| 已提交阶段非法回退 | `E_STATE` | `E_STATE` | 通过 |
| 非法迁移后状态 | `committed` | `committed` | 通过 |
| 注入坏块 | `E_BLOCK` | `E_BLOCK` | 通过 |
| 坏块隔离 | 仅块 1 失败 | committed=69, failed=1, pending=2 | 通过 |
| 坏块重试 | completed | completed | 通过 |
| 坏块重试验证 | verified | true | 通过 |

坏块失败时其他 23 个块均保持已提交；失败块自身仅 `growth` 为 failed，其余两个后续阶段保持 pending，等待恢复。

## 机器可读产物

- `artifacts/acceptance.json`：完整验收报告
- `artifacts/checkpoints.db`：三个正常路径及坏块重试会话的 SQLite 检查点
- `samples/`：seed 42 的三组不同分辨率样本及真值
