# 本地对局回放服务：技术说明与验证记录

纯 Python 标准库实现（`hashlib` / `json` / `os`），无网络依赖、无第三方包、
无容器。Python 3.11+ 即可运行（实际验证环境为 CPython 3.14）。

## 模块结构

- `replay_service/model.py` — 状态模型与确定性哈希。整数定点运算，状态按固定
  字段顺序（`sort_keys` 规范 JSON）序列化后取 SHA-256；哈希链
  `chain[i] = sha256(chain[i-1] + ":" + state_hash[i])`，源起
  `sha256("genesis:v{MODEL_VERSION}")`。哈希真值来源仅为字段序列化结果。
- `replay_service/buffer.py` — 按到达顺序的 per-player 操作缓冲。重复
  `(player, seq)` 只保留首个并记录 `E_OP_DUPLICATE`；超过声明窗口
  （`late_window`）的迟到操作以 `E_OP_LATE` 拒绝并记录；步边界按
  `(tick, arrival)` 稳定排序取出。
- `replay_service/settle.py` — 纯函数结算器：只依赖输入操作与快照状态，
  不读系统时间；越界坐标在状态变更前以 `E_OP_OUT_OF_BOUNDS` 拦截。
- `replay_service/snapshot.py` — 原子快照提交（tmp 文件 + `fsync` +
  `os.replace`），内容含版本号、步号、状态、缓冲水位（每玩家已结算序号）、
  摄入游标与哈希链前缀。写失败不覆盖上一有效快照，报
  `E_SNAPSHOT_WRITE`（含步号与磁盘错误类别）。恢复时校验版本、必填字段、
  逐节校验哈希链并定位首个断链步号，任何缺陷以 `E_RECOVERY_INVALID`
  中止且不产出新状态。
- `replay_service/baseline.py` — 基线逐条比对。版本不符
  `E_BASELINE_VERSION`；缺失/空/行数不足 `E_BASELINE_MISSING`；行损坏
  `E_BASELINE_CORRUPT`；首个不一致输出步号、字段路径
  （如 `chain[19].state_hash`）与两侧取值（`E_BASELINE_DIVERGED`）。
  基线不可用时中止，绝不部分比对。
- `replay_service/inject.py` — 异常注入通道，只作用于数据处理入口
  （操作摄入点 / 磁盘文件），不修改结算器与快照模块源码；每类异常在写出
  新快照前被拦截，注入报告带注入点定位信息。
- `replay_service/runner.py` — 推进任务。每房间一把锁文件（含 stale-pid
  检测），重复启动 `E_BUSY`；步边界检查协作式取消（`CANCEL` 文件或回调），
  取消被消费后清除标记，释放锁与文件句柄并保留最后已提交快照；
  `--crash-at-step` 模拟 kill -9（`os._exit(75)`，无退出报告），恢复只能
  依赖磁盘快照。
- `replay_service/samplegen.py` — 确定性样本生成器：种子驱动，支持玩家数、
  步数、乱序程度参数；乱序由有界局部交换产生（确定性），日志不以本地时间
  排序或去重。
- `replay_service/cli.py` — 命令行入口（`gen/run/resume/verify/inject/cancel`）。

## 业务码一览

| 码 | 含义 |
| --- | --- |
| `E_OP_LATE` | 操作迟到超过声明窗口，拒绝并记录 |
| `E_OP_DUPLICATE` | 重复客户端序号，仅首个生效 |
| `E_OP_OUT_OF_BOUNDS` | 越界坐标，结算前拦截 |
| `E_SNAPSHOT_WRITE` | 快照提交失败（含步号与错误类别） |
| `E_RECOVERY_INVALID` | 快照缺失/截断/字段缺失/版本不符/哈希链断链 |
| `E_BASELINE_VERSION` | 基线模型版本不匹配 |
| `E_BASELINE_MISSING` | 基线缺失或行数不足 |
| `E_BASELINE_CORRUPT` | 基线行损坏/步序断裂 |
| `E_BASELINE_DIVERGED` | 首个分叉点（步号、字段路径、两侧取值） |
| `E_BUSY` | 房间已有推进任务 |
| `E_CANCELLED` | 步边界协作式取消（保留最后已提交快照） |

## 使用方法

```bash
# 生成样本 + 参考推进 + 基线（首次运行即创建本地目录）
python3 -m replay_service.cli gen --seed 7 --players 4 --steps 60 \
    --disorder 0.3 --out artifacts/sample_s7
# 从头推进并逐比对基线
python3 -m replay_service.cli run --sample artifacts/sample_s7/sample.json \
    --room-dir artifacts/room_run1
python3 -m replay_service.cli verify --room-dir artifacts/room_run1 \
    --baseline artifacts/sample_s7/baseline.jsonl
# 模拟强制终止（退出码 75，无退出报告），再从磁盘快照恢复
python3 -m replay_service.cli run --sample ... --room-dir artifacts/room_crash \
    --crash-at-step 25
python3 -m replay_service.cli resume --sample ... --room-dir artifacts/room_crash
# 异常注入
python3 -m replay_service.cli run ... --inject-kind duplicate-op --inject-at-step 6
python3 -m replay_service.cli inject --kind truncated-snapshot --room-dir ...
python3 -m replay_service.cli inject --kind corrupt-baseline --baseline ...
# 协作式取消
python3 -m replay_service.cli cancel --room-dir ...
# 测试
python3 -m unittest discover -s tests -v
```

## 实际验证结果（2026-11-20，CPython 3.14，Linux）

- `python3 -m unittest discover -s tests`：25 个测试全部通过，覆盖
  确定性、缓冲规则、快照/恢复、基线比对、五类注入、并发与取消。
- 样本生成：seed 7（4 玩家/60 步/乱序 0.3）、seed 11（2/30/0.0）、
  seed 23（8/90/0.8）、seed 31（8/4000/0.4）均成功生成样本与基线。
- 确定性：同一 s7 样本两次独立推进，`trace.jsonl` 逐字节一致
  （`cmp` 通过）；链尖哈希
  `b018df63…613a6` 与参考推进一致。
- 崩溃恢复：`--crash-at-step 25` 退出码 75；`resume` 后 60 步全量
  `verify` 为 `match`，且恢复后轨迹与无中断参考轨迹逐字节一致。
- 取消恢复：4000 步任务在步 186 边界响应取消（保留步 185 快照、锁已
  释放），`resume` 续跑至 4000 步后 `verify` 为 `match`；该样本自然产生
  405 条 `E_OP_LATE` 事件（乱序超窗），均被拒绝并记录。
- 注入验证（均返回独立业务码与注入点定位）：
  - `duplicate-op` → 事件日志 `E_OP_DUPLICATE`，链尖与干净基线一致
    （去重生效，未分叉）；
  - `out-of-bounds` → `E_OP_OUT_OF_BOUNDS`，结算前拦截；
  - `late-op` → `E_OP_LATE`；
  - `truncated-snapshot`（快照截断至 64 字节）→ 恢复返回
    `E_RECOVERY_INVALID`（reason=truncated），未产出新状态；
  - `corrupt-baseline`（第 20 行哈希翻转）→ `verify` 返回
    `E_BASELINE_DIVERGED`，定位 `chain[19].state_hash` 并给出两侧取值；
  - 基线缺失 → `E_BASELINE_MISSING`。

## 已知限制

- 快照内含完整哈希链前缀（用于恢复时定位首个断链步号），快照体积随步数
  线性增长，提交为 O(步数²) I/O；4000 步样本约 30 秒。样本规模下可接受，
  生产可改为链分段 + 滚动摘要。
