# 样本工程结构

## 顶层

```json
{
  "schema_version": 1,
  "id": "glass-meridian-20261124",
  "title": "Glass Meridian / Seed 20261124",
  "seed": 20261124,
  "initial_flags": {
    "arrived": false,
    "fragment_count": 0
  },
  "characters": {
    "mara": {
      "name": "Mara Venn",
      "state": {
        "resolve": 1,
        "trust": 0,
        "focus": 2
      }
    }
  },
  "nodes": [],
  "choices": [],
  "truth": {}
}
```

## 节点

```json
{
  "id": "gate",
  "title": "The Convergence Gate",
  "requires": [
    {
      "scope": "flag",
      "key": "fragment_count",
      "op": "gte",
      "value": 3
    },
    {
      "scope": "character",
      "character": "ivo",
      "key": "trust",
      "op": "gte",
      "value": 1
    }
  ],
  "mutex": ["false_gate"],
  "join": ["whisper", "archive", "archive_echo"],
  "events": [
    {
      "id": "ev-gate-flag",
      "kind": "set_flag",
      "key": "gate_open",
      "value": true
    },
    {
      "id": "ev-gate-resolve",
      "kind": "inc_character",
      "character": "mara",
      "key": "resolve",
      "value": 2
    }
  ],
  "shots": [
    {
      "duration_ms": 880,
      "aspect_priority": ["16:9", "4:3"],
      "description": "gate: composition 1"
    }
  ]
}
```

### 条件

- `scope`: `flag` 或 `character`
- `character`: `scope=character` 时必填
- `key`: 标志位或角色状态键
- `op`: `eq`、`ne`、`gt`、`gte`、`lt`、`lte`、`exists`、`not_exists`
- `value`: 除 `exists` / `not_exists` 外必填；排序操作要求数值

### 事件

- `set_flag`: 设置标志位
- `inc_flag`: 数值累加
- `set_character`: 设置角色状态
- `inc_character`: 角色状态数值累加

事件 ID 在全工程内唯一，事件到达顺序按节点声明顺序记录。

### 镜头

- `duration_ms`: 正整数定点毫秒，范围 `1..86400000`
- `aspect_priority`: 非空、去重后的画幅优先级数组
- `description`: 可选展示文本

镜头数量和每镜时长共同决定节点时长统计。呈现时刻由确定性播放时钟生成，不写入工程源文件。

## 选择

```json
{
  "at_ms": 6240,
  "target": "gate",
  "label": "Unseal the convergence gate"
}
```

- `at_ms` 为非负定点整数，数组顺序单调不减。
- `target` 必须存在。
- 目标节点未满足 `join`、`requires` 或 `mutex` 时，选择被消费但记录为 blocked，不重试。

## 真值

```json
{
  "truth": {
    "final_flags": {
      "dawn": true,
      "ending": "dawn"
    },
    "final_characters": {
      "mara": {
        "resolve": 4,
        "trust": 2
      }
    },
    "event_order": ["ev-arrival-flag", "..."],
    "reachable_nodes": ["arrival", "whisper", "archive", "..."],
    "shot_count": 19,
    "total_duration_ms": 16570
  }
}
```

真值由同一引擎在样本生成阶段计算，之后加载样本时再次推进并校验。真值不参与情节推进决策。

## 存档片段

```json
{
  "schema_version": 1,
  "algorithm_version": "story-engine/1",
  "project_id": "glass-meridian-20261124",
  "content_hash": "sha256...",
  "fingerprint": "sha256...",
  "done": false,
  "state": {
    "choice_offset": 4,
    "shot_progress": 9,
    "shots": [],
    "node_states": {},
    "flags": {},
    "characters": {},
    "event_order": [],
    "blocked_choices": [],
    "clock_ms": 8120,
    "current_node_id": "archive",
    "current_node_next_shot": 1,
    "current_choice_index": 3,
    "current_choice_at_ms": 3610,
    "activation_counter": 4
  }
}
```

真实存档包含完整镜头历史与状态映射；这里为缩短说明省略了内容。

