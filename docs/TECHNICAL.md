# 确定性指令流重放器 — 技术说明

## 环境

- Python 3.12.16（`.venv/`，uv 管理），Tkinter 9.0 可正常创建窗口
- NumPy 2.5.5（≥1.24），用于单位位置 / 速度 / 存活标记的整数向量运算
- 工作区根目录含 `samples/`（指令流样本）与 `baseline/`（逐帧基线与轨迹日志）

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `replay/canon.py` | 规范化序列化（sort_keys + 紧凑分隔符 + UTF-8）与 SHA-256 哈希链 |
| `replay/models.py` | 指令模型与战场状态模型（NumPy int64 向量，固定字段顺序展开） |
| `replay/engine.py` | 按玩家缓冲、固定时间步结算、配额强制、哈希链推进、快照载荷、恢复重建 |
| `replay/snapshot.py` | 临时文件 + fsync + `os.replace` 原子提交；创世起逐节校验哈希链 |
| `replay/compare.py` | 基线逐帧比对与首个分叉点诊断（帧号 / 字段路径 / 两侧取值） |
| `replay/runner.py` | 同一对局单推进任务（E_BUSY）、时间步边界协作式取消 |
| `replay/generator.py` | 种子化确定性样本生成器（乱序 / 重复 / 迟到 / 超额可调） |
| `replay/cli.py` | `gensuite` / `gen` / `replay` / `recover` / `compare` 子命令 |

## 关键设计

### 结算顺序（需求 1）
指令按 `(recv_tick, recv_seq, player, client_seq)` 排序摄取；每个时间步边界
先淘汰超窗指令（`t - submit_tick > compensation_window` → `E_CMD_LATE`），
再按 `(recv_seq, player, client_seq)` 稳定顺序结算。重复 `client_seq` 只保留
首个（按接收序号），后续投递记 `E_CMD_DUP`。全程不读系统时间、不用全局随机。

### 哈希链（需求 2）
每步结算后按固定字段顺序序列化单位位置 / 速度 / 存活标记与玩家配额，
`state_hash = sha256(canonical(state))`，
`chain_hash_t = sha256(chain_hash_{t-1} || ":" || state_hash_t)`，
创世链值为 64 个 `0`。真值只来自字段序列化内容。

### 配额强制（需求 3）
每帧配额由输入 `quota_per_frame` 给定（常量或逐帧列表），运行中只读。
帧内累计需求超配额即拒，记 `E_QUOTA_EXCEEDED`（含帧号与玩家标识），
被拒指令不触碰战场状态，配额不被静默提高。

### 原子快照（需求 4）
每步结束写 `step_NNNNNN.json.tmp` → fsync → `os.replace` → 目录 fsync。
快照含步号、战场状态、缓冲水位、各玩家配额累计用量、已结算序号集合、
状态哈希、前向链哈希与哈希链前缀摘要。写失败抛 `E_SNAPSHOT_WRITE`
（带步号与原因），旧快照字节不变。

### 恢复（需求 5）
从最新快照回读，自创世步逐节校验：字段完整性、版本号、状态哈希与内容
一致、链指针连续；任一失败抛 `E_RECOVERY_INVALID` 并定位首个断链步号。
重建缓冲与已结算集合后继续推进，不会重复应用已结算指令；恢复只依赖
磁盘已提交内容。

### 基线比对（需求 6）
逐帧比对 `state_hash / chain_hash / events / quota_used /
buffer_watermark / applied`，输出第一个不一致的帧号、字段路径与两侧
取值，并附该帧生效指令集与缓冲水位。基线版本或状态模型版本不符抛
`E_BASELINE_VERSION` 并终止。

### 并发与取消（需求 7）
`MatchRunner` 以 match_id 加锁登记，重复启动抛 `E_BUSY`；推进循环在每个
时间步边界检查取消事件，取消 / 异常后释放句柄（快照按步写后即关），
磁盘上保留最后一次已提交快照。

### 样本生成（需求 8）
`disorder ∈ [0,3]` 控制乱序洗牌、窗口内 / 超窗迟到、重复投递与超额申请
比例；全部随机性来自 `random.Random(seed)`。相同种子与操作序列下样本、
状态哈希与轨迹日志逐字节一致（日志为规范化 JSONL，无时间戳 / 文件大小）。

## 验证结果（2026-11-21）

- `pytest`：21 项测试全部通过（确定性、去重、迟到、配额、稳定顺序、
  哈希链、快照原子写、写失败保护、恢复续算、断链 / 版本检测、基线匹配
  与分叉诊断、E_BUSY、协作式取消、生成器逐字节一致）。
- `gensuite` 连跑两次，`samples/` 与 `baseline/` 全部文件 sha256 逐字节一致。
- `replay` + `compare`：样本 `s23-p4-f40-d2` 与基线 40 帧全匹配。
- 恢复：样本 `s42-p5-f48-d3` 推进到第 16 步后模拟强制终止，`recover`
  续算至第 48 帧，链顶哈希 `f4586891…dabda8` 与基线一致；恢复段
  31 帧轨迹与基线逐帧匹配。
- 分叉诊断：篡改基线第 9 帧事件后 `compare` 报告
  `frame=9, field=events[0].type` 及两侧取值、该帧生效指令集与缓冲水位。

## 使用

```bash
.venv/bin/python -m replay.cli gensuite                 # 生成样本+基线+轨迹
.venv/bin/python -m replay.cli replay --sample samples/s23-p4-f40-d2.json \
    --snapshots runs/s23/snap --trace runs/s23.trace.jsonl
.venv/bin/python -m replay.cli recover --sample samples/s42-p5-f48-d3.json \
    --snapshots runs/s42/snap --trace runs/s42.recovered.trace.jsonl
.venv/bin/python -m replay.cli compare --trace runs/s23.trace.jsonl \
    --baseline baseline/s23-p4-f40-d2.baseline.json
.venv/bin/python -m pytest -q
```
