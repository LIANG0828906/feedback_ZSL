# 互动叙事因果演算内核

这个工作区实现了一个可复现的本地因果演算内核。它把 JSON 剧本载入为事件图，以显式定点或 IEEE-754 binary64 模式逐步推进，记录每个条件表达式的求值链、误差区间与分支归因，并支持链式哈希轨迹、原子快照恢复和加载入口故障注入。

## 快速开始

环境要求：Python 3.11+、NumPy 2.x。依赖安装完全离线，不访问网络。

```bash
# 从本机已有缓存创建 .venv 并复制 NumPy 2.x
./scripts/bootstrap_offline.sh

# 生成样本、运行固定点/浮点对照、轨迹复放、注入与强制终止恢复验证
./scripts/run_experiment.sh
```

直接运行 CLI：

```bash
.venv/bin/python -m causalkernel.cli generate --out-dir data --seed 20261121
.venv/bin/python -m causalkernel.cli compare \
  --scenario data/branch_flip.json \
  --operations data/operations.json \
  --fixed-places 3 \
  --out-dir reports/scenarios
.venv/bin/python -m causalkernel.cli replay \
  --scenario data/branch_flip.json \
  --trace reports/scenarios/branch_flip.fixed.trace.json
```

## 主要产物

- `data/*.json`：确定性样本与操作序列。
- `reports/validation_report.json`：完整实验验收报告，含每个场景、注入、复放、恢复和确定性结果。
- `reports/RESULTS.md`：便于阅读的验收摘要。
- `reports/scenarios/*.fixed.trace.json` / `*.float.trace.json`：逐决策步轨迹。
- `reports/scenarios/*.comparison.json` / `*.comparison.md`：定点与浮点真值、误差界和分支差异。
- `reports/scenarios/*.attribution.json` / `*.attribution.md`：逐条件误差链和最早越界因子。
- `reports/recovery_snapshots/`：每步原子写入的快照。

## 剧本结构摘要

```json
{
  "format": "causal-story/1",
  "scenario_id": "example",
  "start_event": "start",
  "precision": {"mode": "fixed", "decimal_places": 3, "rounding": "half_even"},
  "initial_state": {
    "characters": {"hero": {"trust": {"kind": "fixed", "text": "1.000", "scale": 3}}},
    "resources": {"energy": {"kind": "fixed", "text": "10", "scale": 0}},
    "flags": {"gate_open": false}
  },
  "events": [
    {
      "id": "start",
      "preconditions": ["trust_positive"],
      "conditions": [
        {
          "id": "trust_positive",
          "expr": {"op": "var", "path": "characters.hero.trust"},
          "op": ">",
          "threshold": {"kind": "fixed", "text": "0", "scale": 0}
        }
      ],
      "effects": [
        {"id": "open", "op": "set", "path": "flags.gate_open", "value": true}
      ],
      "branches": [
        {"id": "b1", "to": "next", "priority": 10, "condition": "trust_positive"}
      ]
    }
  ]
}
```

表达式支持 `const`、`var`、`add`、`sub`、`mul`、`neg`、`pow`；效果支持 `add`、`sub`、`mul`、`set`、`toggle`。所有算术系数必须使用 `{"kind":"fixed"|"float","text":"..."}` 标记。固定点系数还必须声明 `scale`。

`conditions` 是当前事件可被分支引用的条件目录；`preconditions` 才是事件触发前必须为真的条件 ID 列表。完整语义、哈希格式、误差传播公式和归因算法见 `TECHNICAL.md`。

## 错误码

- `E_SCENARIO_INVALID`：剧本结构、缺失目标、非法系数、普通成环或超深图。
- `E_TRACE_INVALID`：轨迹截断、内容哈希或因果链断裂、复放不一致。
- `E_RECOVERY_INVALID`：快照版本、场景、模式、操作、状态或哈希链不一致。
- `E_ATTRIBUTION_FAILED`：条件不确定但无法找到越界因子。
- `E_INJECT_CYCLE`：注入循环事件依赖。
- `E_INJECT_PRECISION_CONFLICT`：注入阈值与定点精度的冲突。
- `E_INJECT_DEPTH_EXCEEDED`：注入超深分支链。
- `E_INJECT_NONFINITE_COEFFICIENT`：注入非有限系数。
- `E_INJECT_TRUNCATED`：注入截断剧本。

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

当前 10 个单元测试覆盖双模式分支翻转、归因、全图校验、优先级稳定排序、效果与状态哈希、逐字节确定性、轨迹复放与损坏拒绝、五类注入、原子写入以及强制终止后的快照恢复。
