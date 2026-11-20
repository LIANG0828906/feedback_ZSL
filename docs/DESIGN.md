# 配额确定性重演服务 — 技术说明

## 1. 模块结构

| 模块 | 职责 | 对应业务要求 |
| --- | --- | --- |
| `replay_service/errors.py` | 业务错误码（`E_INPUT_INVALID` 等）与 `ReplayError` | 全部 |
| `replay_service/models.py` | 配置/操作/结果数据模型与规范序列化 `canonical_dumps` | 2、6、8 |
| `replay_service/parser.py` | 本地 JSON 解析与严格字段校验，定位首个出错记录 | 1 |
| `replay_service/engine.py` | 按逻辑步推进的结算引擎、稳定排序、哈希链 | 2、3、6 |
| `replay_service/checkpoint.py` | 临时文件 + `os.replace` 原子检查点、续算与断链定位 | 4、5 |
| `replay_service/runner.py` | 单演练串行重演、并行工作池、暂停/协作式取消 | 2、6、8 |
| `replay_service/baseline.py` | 基线生成、版本闸门与首个分叉归因 | 7 |
| `replay_service/generator.py` | NumPy 固定种子样本/基线/轨迹生成 | 8 |
| `replay_service/gui.py` | Tkinter 复核界面（无 tk/无显示时优雅报错） | 8 |
| `replay_service/cli.py` | 脚本化入口：`generate/replay/verify/resume/gui` | 验收 |

## 2. 输入契约（`input.json`）

```json
{
  "config": {
    "exercise_id": "ex-1",
    "total_steps": 20,
    "per_step_quota": 12.0,
    "player_ids": ["P00", "P01"],
    "penalty_rule": {
      "policy": "reject",
      "reject_penalty": 2.0,
      "degrade_rate": 1.5,
      "degrade_fixed": 1.0
    }
  },
  "operations": [
    {
      "op_id": "op-000001",
      "player_id": "P00",
      "client_seq": 7,
      "effective_step": 3,
      "required_compute": 4.5,
      "client_ts": 310.0,
      "arrival_ts": 317.0
    }
  ]
}
```

校验规则（任一不满足即 `E_INPUT_INVALID`，错误上下文 `location` 指向首个出错位置）：

- 必需字段缺失不补默认值；布尔值不接受为数字；
- `total_steps > 0`；`effective_step ∈ [0, total_steps)`；
- `required_compute` 与所有惩罚参数非负；
- `player_id` 必须在配置玩家集合内；
- 同一 `(player_id, client_seq)` 不得重复；
- 惩罚策略仅允许 `reject` / `degrade`。

`client_ts`、`arrival_ts` 仅用于延迟观测，解析后不进入结算、惩罚、检查点与任何哈希。

## 3. 结算语义

每一步：

1. 取出生效步等于当前步的全部操作，按 `(player_id, client_seq)` 稳定排序；
2. 先汇总每玩家“尝试总量” `attempted_total`；
3. `reject` 策略：按客户端序号逐条投影 `running + required`，一旦 `> quota`
   即拒绝该条并按 `reject_penalty` 计一次扣减；每个透支玩家产生一条
   `QuotaEvent`，带步号、玩家、尝试总量、上限、被拒 `op_id` 列表；
4. `degrade` 策略：操作全部生效，对 `attempted_total - quota` 的超出量按
   `degrade_fixed + degrade_rate * overage` 计一次整步扣减；
5. 累计本步与全局用量/惩罚，计算步骤哈希并追加哈希链。

严格模式（`replay_exercise(..., strict_quota=True)`）在透支步直接抛
`E_QUOTA_EXCEEDED`，上下文包含 `step/player_id/attempted_total/quota/policy/
rejected_op_ids/op_client_seqs`；默认模式只记录事件、继续重演，便于离线归因。

## 4. 确定性保证

- 排序键只有 `(player_id, client_seq)`，输入数组顺序（模拟到达顺序）不影响结果；
- 规范序列化：`json.dumps(..., sort_keys=True, separators=(",", ":"),
  ensure_ascii=False)`，同一语义对象逐字节一致；
- 步骤哈希负载固定为
  `{step, prev_hash, cumulative_used, cumulative_penalty, penalties, op_client_seqs}`
  的规范 JSON 的 SHA-256，不含时间戳、文件大小、线程 id、对象 id；
- 引擎不调用 `time/datetime/random`；样本随机全部来自
  `numpy.random.default_rng(seed)`，种子派生自场景名；
- 并行使用 `ThreadPoolExecutor`，每场演练在单个任务内严格串行；
  结果按输入下标回填，完成先后不影响输出顺序与内容。

## 5. 检查点与续算

- 文件名固定 `checkpoint.json`，写入走“同目录临时文件 → flush/fsync →
  `os.replace` 原子重命名”；任何失败都清理临时文件并抛
  `E_CHECKPOINT_WRITE`（带步号），上一有效检查点保持不变；
- 检查点含版本号、规则版本、`step_index`、各玩家累计用量/惩罚、
  逐步 `step_records`（含每步 `prev_hash/step_hash`）、缓冲水位
  `buffer_watermark`（已结算操作条数）；
- 续算先做版本/规则/exercise_id/必需字段校验，再从创世哈希
  `0x00…00` 重放整条哈希链并重算每步哈希，首个断链/篡改步通过
  `E_RESUME_INVALID` 的 `broken_step` 上报；校验失败发生在任何新写出之前；
- 恢复的引擎从 `step_index` 之后继续，已提交步不会再次进入累计。

## 6. 分叉归因

`compare_with_baseline` 先校验 `baseline_version` 与 `rule_version`
（不符即 `E_BASELINE_VERSION`，不给分叉结论），再逐步行比对
`cumulative_used`、`cumulative_penalty` 与 `step_hash`，返回首个不一致的
逻辑步、玩家、字段、两侧取值，并附该步参与结算的
`{player_id, client_seq}` 序列，可直接定位到导致分叉的操作顺序。

## 7. 轨迹日志与 GUI

- 轨迹 `trajectory.jsonl`：每玩家每步一行规范 JSON，字段固定
  `step/player_id/step_used/cumulative_used/step_penalty/
  cumulative_penalty/step_hash`，相同输入与任意并行度逐字节一致；
- GUI 基于 Tkinter，提供启动/暂停/继续/协作式取消、进度条、
  每演练末步哈希与累计扣减、透支/惩罚/分叉事件列表；
  环境缺少 tkinter 或无显示时抛明确 `RuntimeError`，脚本化 CLI 不受影响。

## 8. 本地验证入口

```bash
python -m replay_service.cli generate --out-dir samples/run1 --seed 20261121
python -m replay_service.cli replay  --sample-dir samples/run1 --work-dir /tmp/w1 --parallelism 1
python -m replay_service.cli replay  --sample-dir samples/run1 --work-dir /tmp/w3 --parallelism 3
python -m replay_service.cli verify  --sample-dir samples/run1 --work-dir /tmp/v  --parallelism 4
python -m replay_service.cli resume --input samples/run1/large_degrade/input.json \
    --checkpoint-dir /tmp/w1/checkpoints/large_degrade
python -m unittest discover -s tests -v
python -m scripts.verify_kill_resume
```
