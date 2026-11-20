# 离线确定性关卡生成工作台

这是一个不依赖网络、账号或外部数据集的 Python 3.11 关卡生产管线。输入由种子、参数与带 ID 的约束集合组成，输出包含房间矩形、方向门、起点/终点/钥匙/锁/奖励标签、可复放轨迹和业务诊断。

## 快速开始

### Python 3.11 与依赖

目标环境固定为：

- Python 3.11
- numpy 1.26.4
- PySide6 6.7.0

Windows PowerShell 示例：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python -c "import numpy, PySide6; print(numpy.__version__, PySide6.__version__)"
python scripts/check_env.py
```

Linux/macOS 示例：

```bash
python3.11 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
python -c "import numpy, PySide6; print(numpy.__version__, PySide6.__version__)"
```

本仓库实际验证环境为 Python 3.11.18、numpy 1.26.4、PySide6 6.7.0。生成器本身只使用标准库；numpy 按生产环境要求固定，PySide6 仅用于图形工作台。

### 生成与复放

```bash
# 生成。成功写出 level.json、trace.json、result.json
python -m levelgen.cli generate \
  --request artifacts/requests/s24_std.request.json \
  --out artifacts/run_s24

# 从轨迹复算并断言终局哈希；失败时不写 level.json
python -m levelgen.cli replay \
  --trace artifacts/traces/s24_std.trace.json \
  --out artifacts/replay_s24

# 无显示入口，也支持 generate/replay
python scripts/run_headless.py replay \
  --trace artifacts/traces/s24_std.trace.json \
  --out artifacts/replay_headless
```

### 样本、基线与本机回归

```bash
# 清空并重建 artifacts，随后校验确定性、冲突重建和注入拦截
python -m levelgen.cli samples --out artifacts
python -m levelgen.cli verify --artifacts artifacts
python -m unittest discover -s tests -v
```

`samples` 会清空目标输出目录。样本包括三档网格规模、两档约束强度、三种静态冲突和五种注入。

### 图形工作台

```bash
python -m levelgen.gui
```

无显示环境可验证 GUI 实际走的后台生成/复放线程路径：

```bash
QT_QPA_PLATFORM=offscreen python -m levelgen.gui \
  --offscreen-smoke \
  --request artifacts/requests/s24_std.request.json \
  --out /tmp/gui_generate

QT_QPA_PLATFORM=offscreen python -m levelgen.gui \
  --offscreen-smoke \
  --trace artifacts/traces/s24_std.trace.json \
  --out /tmp/gui_replay
```

Windows 无显示冒烟可使用 `$env:QT_QPA_PLATFORM="offscreen"`。工作台画布显示网格、房间、方向门和标签；侧栏可编辑种子、参数、约束 ID/类型/硬软标记/JSON 参数，并浏览轨迹条目、复放轨迹。生成与复放在 `QThread` 中执行。

## 请求格式

```json
{
  "seed": 2401,
  "params": {
    "grid_w": 24,
    "grid_h": 24,
    "rewards": 1,
    "max_attempts": 32
  },
  "constraints": [
    {
      "id": "c_rooms",
      "kind": "room_count",
      "hard": true,
      "params": {"min": 9, "max": 9}
    },
    {
      "id": "c_chain",
      "kind": "key_chain",
      "hard": true,
      "params": {"keys": ["K1", "K2"]}
    }
  ]
}
```

可选尺寸参数为 `room_min_w`、`room_max_w`、`room_min_h`、`room_max_h`，默认分别为 `2,5,2,5`。约束数量上限为 32。

### 约束类型

| kind | 参数 | 语义 |
| --- | --- | --- |
| `room_count` | `min,max` | 房间数量硬区间；或软目标 |
| `connectivity` | 无 | 门图在忽略方向时连通 |
| `key_chain` | `keys:[...]` | 钥匙按给定顺序可达，且后续钥匙在前序锁打开前不可达 |
| `end_locked` | 无 | 无钥匙时终点不可达；取得全部钥匙后终点可达 |
| `area_target` | `target,tolerance` 或 `min,max` | 房间总面积目标 |
| `door_count_target` | `target,tolerance` 或 `min,max` | 门数量目标 |

每个约束必须有唯一 `id` 和布尔 `hard`。硬约束失败阻止产出；软目标失败只记录在轨迹 `soft` 检查中。

## 关卡与判定

关卡 JSON 兼容以下核心结构：

```json
{
  "grid": {"w": 24, "h": 24},
  "rooms": [{"id": 0, "x": 3, "y": 4, "w": 5, "h": 3}],
  "doors": [{"id": 0, "a": 0, "b": 1, "dir": "AB"}],
  "labels": [
    {"kind": "start", "room": 0},
    {"kind": "key", "room": 2, "tag": "K1"},
    {"kind": "lock", "room": 3, "tag": "K1"},
    {"kind": "reward", "room": 4},
    {"kind": "end", "room": 5}
  ],
  "meta": {"generator": "levelgen/1.0"}
}
```

门方向支持 `BIDIR`、`AB`、`BA`。锁标签标记受保护房间，进入带锁房间需要对应钥匙标签；该规则只实现于 `levelgen/reachability.py` 的 `traverse`，生成阶段校验、`end_locked` 检查、钥匙依赖环检测和复放诊断全部复用它。

`analyze_reachability` 返回：

- 无锁状态下可达房间和终点可达性；
- 每一把钥匙的可用钥匙集合、可达性和经过门序列；
- 全钥匙状态下的房间序列、门序列、解锁顺序和终点可达性；
- `key_chain_ok` 与 `end_after_all_keys`。

## 确定性、阶段与回滚

生成器只接受 `(seed, constraints, params)`。随机源为基于 SHA-256 派生的 SplitMix64，不使用 `random` 全局状态、系统时间或进程相关种子。随机流按 `attempt + stage` 独立派生，因此某一阶段的抽样不会无解释地改变另一阶段。

阶段顺序：

1. `layout`：选择房间数、尺寸和非重叠位置。
2. `doors`：建立连通骨干，并在不绕过锁割集的前提下分配额外门和方向。
3. `labels`：放置起点、钥匙、锁、奖励和终点。
4. `keychain_validation`：结构校验与全部约束检查。

每个阶段开始前保存完整关卡快照。阶段异常时写入 `stage_failure`，恢复快照后写 `stage_rollback`，再进入下一次确定性尝试。只有全部硬约束通过后才写 `final`。

## 轨迹与复放

轨迹格式为 `levelgen-trace/1`，每条记录包含 `seq`、`prev`、类型内容和 `hash`。`hash` 是对除自身外整条记录的规范 JSON SHA-256；首条 `prev` 为 64 个零。轨迹记录：

- `stage_start` / `stage_end` / `stage_failure` / `stage_rollback`；
- `rng`：方法、参数、抽样结果和派生的流名；
- `op`：放置房间、分配门、放置标签；
- `check`：约束 ID、结论、证据房间/门和可读消息；
- `final`：终局关卡内容哈希；
- `conflict`：可独立重建的冲突报告。

复放不是“相信轨迹里的关卡”。`replay_trace` 先从轨迹中取出唯一输入重新执行确定性生成，再逐条比较完整记录和终局哈希。哈希断链、内容被改、复算结果漂移或终局哈希不一致都会抛出 `E_TRACE_INVALID`，文件服务会删除/不写 `level.json`。

## 冲突诊断

失败报告至少包含：

```json
{
  "attempts": 8,
  "constraint_ids": ["c_chain", "c_rooms"],
  "evidence": {"rooms": [0, 3, 5], "doors": [2, 4]},
  "message": "房间数量区间 ...",
  "stage": "keychain_validation"
}
```

静态矛盾会在 `static` 阶段直接定位，例如：

- 两个 `room_count` 区间交集为空；
- 钥匙链所需最少房间数超过房间上界；
- `end_locked` 存在但没有 `key_chain`；
- 硬面积或硬门数量目标低于连通/最小面积下限。

运行期冲突的报告也写入轨迹 `conflict`。`artifacts/reports/*.trace.json` 可通过 `replay_trace` 独立重建同一报告。

## 异常注入与业务码

注入通道不修改生成器源码。请求级异常在生成前拦截；关卡级异常在内存中生成后、任何关卡写出前经过同一结构校验器拦截。

| 注入 | 业务码 | 典型定位 |
| --- | --- | --- |
| 截断输入 | `E_INPUT_TRUNCATED` | `request` |
| 约束数量超限 | `E_CONSTRAINT_LIMIT` | `request.constraints` |
| 房间越界 | `E_ROOM_OUT_OF_BOUNDS` | `imported_level.rooms[0]` |
| 悬空门端点 | `E_DOOR_DANGLING` | `imported_level.doors[0]` |
| 环状钥匙锁依赖 | `E_KEY_CYCLE` | `imported_level.labels` |
| 轨迹损坏/断链 | `E_TRACE_INVALID` | 具体 `trace.entries[i].*` |
| 工作台重复提交 | `E_BUSY` | `workbench.task` |
| 协同取消 | `E_CANCELLED` | `generation.checkpoint` |

执行单类注入：

```bash
python -m levelgen.cli inject \
  --request artifacts/requests/s24_std.request.json \
  --type key_lock_cycle \
  --out artifacts/inject_one
```

注入目录只写 `result.json`，不写关卡。结果同时包含 `injections_applied` 和错误定位点。

## 工作台并发与取消

`SingleTaskGate` 是纯 Python 单任务门闩，GUI 的生成和复放共用它。任务活动时再次触发返回 `E_BUSY`。取消使用 `threading.Event`，在阶段边界和每次随机抽样前检查，因此不会留下部分关卡；JSON 写出使用同目录临时文件、`fsync`、`os.replace`，异常路径关闭并删除临时文件。

## 本机实际验证结果

验证命令：

```bash
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m levelgen.cli samples --out artifacts
.venv/bin/python -m levelgen.cli verify --artifacts artifacts
QT_QPA_PLATFORM=offscreen .venv/bin/python -m levelgen.gui \
  --offscreen-smoke --request artifacts/requests/s24_std.request.json \
  --out /tmp/gui_generate
QT_QPA_PLATFORM=offscreen .venv/bin/python -m levelgen.gui \
  --offscreen-smoke --trace artifacts/traces/s24_std.trace.json \
  --out /tmp/gui_replay
```

实际结果：

- Python 3.11.18；numpy 1.26.4；PySide6 6.7.0 可导入。
- 12 项 unittest 全部通过。
- 三份样本的生成、字段逐字节稳定、轨迹字节稳定、复放终局哈希一致。
- 三种冲突均可从各自轨迹重建相同报告，且报告包含 `stage` 与 `constraint_ids`。
- 五种注入均返回独立业务码和定位点，注入目录无 `level.json`。
- GUI 无显示后台生成与后台复放均返回 `level_loaded: true`。

样本基线见 `artifacts/baselines/hashes.json`，完整检查明细见 `artifacts/verification_report.json`。当前样本关卡哈希：

| 样本 | 关卡哈希 |
| --- | --- |
| `s16_easy` | `02291c354a6af6d9f158f99f2a1c2ba406cdd2c218b173bb32968b5def1d407e` |
| `s24_std` | `664e5f091da22f444d2145950d61e40a36841eed19ccb3709e57d99e0d7a7b85` |
| `s32_strict` | `0e8c6376ba613ee667deef922e7ee8ac4898422e266451327f5addd0932d0fe8` |

## 目录

```text
levelgen/
  models.py        关卡、请求、约束数据模型
  validation.py    静态约束、结构校验、约束检查
  reachability.py  唯一可达性与解锁顺序实现
  rng.py           确定性 SplitMix64
  generator.py     分阶段生成、回滚、诊断
  trace.py         哈希链、轨迹校验、复放
  injection.py     五类异常注入
  service.py       原子文件写出与脚本编排
  gui.py           PySide6 工作台
  workbench.py     单任务门闩
  samples.py       样本、基线与回归
tests/             标准库 unittest
artifacts/         样本关卡、轨迹、冲突报告、注入结果、基线
```
