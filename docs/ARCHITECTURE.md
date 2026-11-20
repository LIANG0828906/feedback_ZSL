# 本地赛事复盘回放管线 — 技术说明

## 1. 目标与范围

把三种年代的多人协作对战录像（v1/v2/v3）归一为同一内部状态模型，在固定时间步下
确定性结算操作，产出实体位置/速度/存活标记与房间资源计数，构建可复现的状态哈希链，
支持原子快照恢复、基线分叉诊断、异常注入验证与 PyQt5 交互界面。全程本地离线，
不访问任何线上服务。

## 2. 环境与离线依赖

- 解释器：Python 3.14.4（要求 >= 3.10）。
- 依赖：numpy 2.5.5、PyQt5 5.15.11（连带 PyQt5-Qt5 5.15.19、PyQt5-sip 12.20.1）。
- 离线源：`scripts/fetch_wheels.py` 一次性把匹配当前解释器的 wheel 拉到 `wheelhouse/`，
  随后用纯离线命令安装，运行期不触网：

```bash
python3 scripts/fetch_wheels.py
uv venv --python /usr/bin/python3.14 .venv
UV_OFFLINE=1 uv pip install --offline --find-links wheelhouse --python .venv/bin/python numpy PyQt5
```

`uv` 在毫秒级完成解析即证明全部包来自本地 wheelhouse。

## 3. 模块结构

- `replay/errors.py`：业务码枚举与带 record_index/field_path/injection_point 的异常。
- `replay/models.py`：Operation/RoomState/NormalizedReplay 与规范化 JSON、numpy 移动。
- `replay/parser.py`：v1/v2/v3 严格解析与归一，缺失字段/未知版本/越界引用即 E_FORMAT_INVALID。
- `replay/diff_codec.py`：v3 字典序差分编解码、字段字典序校验、逐字段比对。
- `replay/engine.py`：分玩家操作缓冲、固定步结算、重复去重、迟到判定、确定性哈希链。
- `replay/snapshot.py`：临时文件加 os.replace 原子提交、全量重放校验的快照恢复。
- `replay/baseline.py`：基线加载、版本校验、首个分叉点定位。
- `replay/injector.py`：数据处理入口的六类异常注入通道（不改结算器/快照源码）。
- `replay/pipeline.py`：编排、同房间单任务锁、暂停与协作式取消。
- `replay/generator.py`：单种子驱动的三版本录像、基线、轨迹日志生成。
- `replay/gui.py`：PyQt5 界面（懒导入，无头用 QT_QPA_PLATFORM=offscreen）。
- `replay/cli.py`：generate / run 命令行。

## 4. 关键设计

### 4.1 跨版本归一

- v1：仅操作与时间步，快照缺失，全部由操作重放推导，来源标注 derived:operation-replay。
- v2：每记录携带完整快照与事件流，来源标注 recorded。
- v3：每 tick 首条记录携带锚定上一 tick 的差分与快照，按字段字典序解码，
  来源标注 recorded:v3-diff；解码结果与携带快照逐字段比对，不一致即 E_DIFF_INVALID。

### 4.2 确定性结算

- 操作按 (arrival_seq, player_id, client_seq) 稳定排序，与输入解析顺序无关。
- 同玩家重复 client_seq 仅首个保留；目标 tick 低于 current_tick - late_window
  的操作以 E_OP_LATE 拒绝并记录。
- 状态经 sort_keys 规范化 JSON 序列化；哈希为
  sha256(prev_hash + 规范化状态字节)，仅依赖字段内容，不含时间戳或文件大小。

### 4.3 快照与恢复

- 每步写 .tmp 后 fsync 再 os.replace，崩溃只可能残留 .tmp，不会出现半截正式快照。
- 快照含步号、状态、操作缓冲（含已见序号集合）、水位、prev_hash、state_hash。
- verify_and_recover 从初始状态确定性重放到最近快照，逐跳比对完整哈希链，
  支持间隔落盘并在首个断链步抛 E_RECOVERY_INVALID；恢复后从快照缓冲继续，不重复应用。

### 4.4 分叉诊断

逐条比对基线哈希，首个不一致步输出步号、首个叶子字段路径、两侧取值、该步生效操作集
与缓冲水位；基线模型版本不符返回 E_BASELINE_VERSION，文件缺失返回 E_BASELINE_MISSING。

## 5. 业务码映射

- E_FORMAT_INVALID：字段缺失、类型错误、差分引用越界、注入声明但无故障。
- E_FORMAT_VERSION_UNKNOWN：录像 format_version 不在 {1,2,3}。
- E_FORMAT_ORDER：注入的录像记录 tick 错序。
- E_OP_LATE：操作超过声明迟到窗口（自然迟到与注入共用）。
- E_OP_DUPLICATE：入口注入的重复客户端序号。
- E_OP_OUT_OF_BOUNDS：注入坐标超出 [-1000,1000]。
- E_DIFF_INVALID：差分 base 不可解析、字段字典序不匹配、快照比对不符。
- E_DIFF_TRUNCATED：入口注入的被截断 v3 差分。
- E_RECOVERY_INVALID：快照字段缺失、链断链、版本不符、注入损坏快照。
- E_BASELINE_VERSION / E_BASELINE_MISSING：基线版本不符 / 基线缺失。
- E_BUSY：同房间重复启动，或协作式取消。

自然出现的重复序号在缓冲层去重并记录（不报错）；显式注入的重复序号在入口直接
返回 E_OP_DUPLICATE，二者语义分离。

## 6. 运行方式

```bash
# 生成确定性三版本样本 + 基线 + 轨迹日志
.venv/bin/python -m replay.cli generate --out data/demo --players 4 --ticks 24 --seed 20261121

# 回放并比对基线
.venv/bin/python -m replay.cli run data/demo/replay_v3.json \
  --snapshot-dir data/demo/snap --baseline data/demo/baseline.json

# 异常注入（六类）
.venv/bin/python -m replay.cli run data/demo/replay_v1.json \
  --snapshot-dir data/demo/fault --fault duplicate_op --fault-record 1

# 图形界面（无头环境加 offscreen）
QT_QPA_PLATFORM=offscreen .venv/bin/python -m replay.gui \
  data/demo/replay_v3.json data/demo/gui_snap data/demo/baseline.json

# 单元测试与端到端验收
.venv/bin/python -m unittest discover -s tests
.venv/bin/python scripts/acceptance_demo.py
```

## 7. 验证结果

- 单元测试：33 个用例全部通过（解析校验、缓冲去重/迟到、跨版本字节一致、
  差分往返与中止、原子快照与恢复、六类注入、基线分叉、单任务锁）。
- 端到端：scripts/acceptance_demo.py 全部断言通过，三版本哈希链逐字节一致，
  同种子重生成文件逐字节一致，断点恢复从 tick 12 续推 12 步，
  分叉精确定位到 tick 7 的 resources.ammo，六类注入返回独立业务码且无状态文件提前写出。
