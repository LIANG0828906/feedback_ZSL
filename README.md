# 战术场景查询核心

这是一个可在无 GUI 环境运行、供回合制战术编辑器调用的确定性场景查询核心。它覆盖本地二进制场景、投影裁剪、屏幕分桶框选、覆盖率、悬停遮挡、预算降级、版本发布、并发编辑隔离与哈希链轨迹回放。

核心不依赖 Qt 进行查询；NumPy 负责矩阵和几何向量运算，PySide6 可作为编辑器前端依赖。所有结果均按 `unit_id` 和固定字段顺序规范化，不使用系统时间戳参与一致性判断。

## 快速开始

Python 3.11+：

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python numpy pyside6 pytest

# 清理并生成本地样本，包括 64、2048、20000 单位和边界/歧义场景
.venv/bin/python -m tactical_scene generate \
  --output artifacts/samples --seed 20261122 --sizes 64,2048,20000

# 复算基线并逐条回放轨迹
.venv/bin/python -m tactical_scene verify --suite artifacts/samples

# 对最大样本执行分档预算基准
.venv/bin/python -m tactical_scene benchmark \
  --suite artifacts/samples --budgets 5,30,60,100,150,250,500 --repeats 5

# 自动化测试
.venv/bin/python -m pytest -q
```

Windows 上将 `.venv/bin/python` 替换为 `.venv\Scripts\python.exe`，命令参数相同。实现只使用 `pathlib`、`os.replace`、`os.fsync` 等跨平台接口。

## 公共 API

```python
from tactical_scene import Camera, BudgetConfig, SceneEngine
from tactical_scene.versioning import load_published_scene

scene = load_published_scene(
    "artifacts/samples/front_020000/scene.bfscene",
    "artifacts/samples/front_020000/manifest.json",
    expected_version=1,
    expected_hash="从发布清单或场景对象取得的 64 位 SHA-256",
)
camera = Camera.look_at(
    (0, 0, 180), (0, 0, 0), (0, 1, 0),
    width=1600, height=900, near=0.5, far=5000,
)
engine = SceneEngine(
    scene,
    budget_config=BudgetConfig(
        budget_ms=250,
        memory_budget_bytes=128 * 1024 * 1024,
    ),
)

handle = engine.acquire_handle()
selection = engine.select_rect(
    camera, 100, 140, 1320, 720, handle=handle,
    expected_version=1, expected_hash=scene.content_hash,
)
hover = engine.hover(
    camera, 800, 450, handle=handle,
)
```

`select_rect` 返回：

- `selected`：规范排序的入选单位编号。
- `coverage_by_unit` / `decisions`：覆盖率、相交面积、投影面积、深度和逐单位原因。
- `scene_version` / `scene_hash` / `camera_hash`：单次结论绑定的场景与相机。
- `strategy`、`bucket_grid`、`refinement_depth`、`stage_counts`、`degraded_stages`。
- `total_elapsed_ms`、各阶段耗时、内存占用、`within_budget`。
- `coverage_error_upper_bound`：多边形精化为 `0.0`，包围盒近似为保守上界 `1.0`。

`hover` 返回最近可见单位、最近面深度、目标可见面积、遮挡单位列表、深度候选栈及区域采样误差上界。深度差小于等于声明的 `depth_epsilon` 时不静默选择，抛出 `E_OCCLUSION_AMBIGUOUS`。

## 错误码

- `E_SCENE_INVALID`：字段缺失、校验和错误、变换不可逆、包围体退化或版本字段无效；可定位时带 `unit_id`/`unit_index`。
- `E_BUDGET_EXCEEDED`：没有计划满足时间/内存预算，或实测执行超过预算。
- `E_OCCLUSION_AMBIGUOUS`：悬停候选在声明深度精度内无法稳定排序。
- `E_SCENE_STALE`：场景版本/哈希与发布清单、期望值或历史快照不一致。
- `E_STALE_HANDLE`：快照句柄在新版本发布后失效。
- `E_BUSY`：同一场景已有编辑任务。
- `E_TRACE_INVALID`：轨迹哈希链断裂、记录被篡改或场景版本字段缺失。

所有错误均为 `TacticalError`，包含稳定的 `.code`、`.message` 和 `.details`。

## 验证结果

本地环境：Python 3.11.18、NumPy 2.4.6、PySide6 6.12.0（Linux/WSL；依赖导入已验证）。当前测试结果：

```text
26 passed
```

正式样本套件 `artifacts/samples` 包含 64、2048、20000 单位场景，正面/斜视/边界后方/正交遮挡歧义相机，每例含 `.bfscene`、发布清单、相机、基线、轨迹和期望歧义记录。`verify` 对全部基线逐字段复算，并对每个轨迹事件按版本回放，结果一致。

详细设计、格式和实测预算数据见 [`docs/TECHNICAL.md`](docs/TECHNICAL.md)。
