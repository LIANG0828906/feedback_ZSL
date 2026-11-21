# 数字乐谱表现求解引擎

本地、确定性、可恢复的音乐游戏乐谱导入与演奏表达管线。项目实现 v1/v2/v3 三种乐谱格式的统一解析、显式版本转换、逐音符表现求解、误差校验、不可变版本发布、增量重解、按声部崩溃恢复，以及 PySide6 图形界面。

## 交付内容

- `score_engine/parser.py`：v1/v2/v3 解析与单位、拍号、起始偏移归一化。
- `score_engine/conversion.py`：升级/降级及 `E_VERSION_LOSS` 显式损失报告。
- `score_engine/solver.py`：速度分段积分、表情插值、力度、连奏/断奏、跨小节踏板求解。
- `score_engine/performance_validation.py`：逐音符时间/力度/音域偏差、容差标记与最大偏差定位。
- `score_engine/publish.py`：内容哈希、发布清单与锁内整体替换的不可变快照。
- `score_engine/incremental.py`：依赖失效、受影响音符集、缓存复用与一次整体发布。
- `score_engine/recovery.py`：声部临时文件 + 原子重命名提交、版本与哈希链校验、断点续算。
- `score_engine/sample_generator.py`：固定种子样本、基线序列、增量/恢复轨迹。
- `score_engine/gui.py`：乐谱网格、偏差热力、表情/速度曲线、踏板、版本时间轴与协作式取消。
- `tests/`：24 项自动化测试。

设计说明见 `docs/DESIGN.md`，实际验收记录见 `docs/ACCEPTANCE.md`。

## 环境

当前已验证环境：

- Python 3.14.4
- NumPy 2.5.5
- PySide6 6.12.0
- pytest 9.2.0
- Linux 本地显示 `DISPLAY=:0`，无 Docker/容器

依赖已安装在项目本地 `.venv`：

```bash
uv venv --python /usr/bin/python3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

若系统 Python 已带 `venv`/`pip`，也可使用：

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

运行时不访问网络、真实乐谱账号或外部音频素材。所有样本、基线、快照和日志均由本地生成器产出。

## 快速运行

生成确定性样本、基线、恢复快照和轨迹日志：

```bash
.venv/bin/python -m score_engine demo --out artifacts/acceptance --seed 20261122
```

运行全部测试：

```bash
.venv/bin/python -m pytest -q
```

无窗口验证 GUI 管线：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m score_engine gui \
  --score artifacts/acceptance/scores/virtual_v3.json --smoke
```

启动图形界面：

```bash
.venv/bin/python -m score_engine gui \
  --score artifacts/acceptance/scores/virtual_v3.json
```

一键复现完整验收并写出 `artifacts/acceptance/acceptance.log`：

```bash
scripts/run_acceptance.sh
```

## 常用命令

```bash
# 求解并校验
.venv/bin/python -m score_engine solve \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --output artifacts/acceptance/manual.performance.json \
  --manifest-dir artifacts/acceptance/manual-manifests

# 校验已有序列
.venv/bin/python -m score_engine validate \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --sequence artifacts/acceptance/baselines/virtual_v3.performance.json

# 升级 v1 -> v3
.venv/bin/python -m score_engine convert \
  --score artifacts/acceptance/scores/classic_v1.json \
  --to 3 --output artifacts/acceptance/manual-upgraded.json

# 降级 v3 -> v2，无 --allow-loss 时返回 E_VERSION_LOSS
.venv/bin/python -m score_engine convert \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --to 2 --allow-loss \
  --output artifacts/acceptance/manual-v2.json

# 强制终止后恢复演示
.venv/bin/python -m score_engine recover-demo \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --out artifacts/acceptance/recovery-demo --crash-after 2
```

## 格式约定

内部统一使用“四分音符拍”为时间轴，所有拍位置与时长用精确有理数保存；外部文件中的音符/踏板/表情位置相对 `start_offset` 记录，解析后加上偏移，导出时减回。

| 版本 | 音符时间单位 | 拍号 | 起始偏移 | 额外表达 |
|---|---|---|---|---|
| v1 | `duration_unit`，默认 whole note | 单一拍号 | `start_offset_seconds` | 仅 pitch/start/duration |
| v2 | `time_unit`，默认 quarter beat，也可 tick | `meter_changes` | `origin_offset_beats` | velocity、articulation、pedal_spans |
| v3 | `{value, unit}`，默认 quarter beat | 多拍号变化 | `start_offset` | 表情锚点、速度段、声部分组、音域 |

业务错误：

- `E_SCORE_INVALID`：字段缺失、未知版本、时值与拍号网格矛盾；定位 `note_id`、`field`、`part_id`。
- `E_VERSION_LOSS`：目标版本无法表达属性；逐音符列出字段，不静默丢弃。
- `E_VERSION_STALE`：查询的版本号或内容哈希与当前发布不一致。
- `E_RECOVERY_INVALID`：恢复快照缺字段、版本不符或哈希链断开；定位首个断点。
- `E_CANCELLED`：协作式取消。

## 结果位置

`artifacts/acceptance/` 包含：

- `scores/`：v1/v2/v3 原生样本、升级组合、降级组合、音域越界样本。
- `baselines/`：每个样本的基线表现序列与增量结果。
- `recovery/`：按声部提交的恢复快照。
- `manifests/`：基线清单、转换报告、增量发布清单。
- `trace.jsonl`：确定性轨迹日志，包含版本、声部数、受影响/复用音符数、偏差与内容哈希。

