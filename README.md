# 手绘风格分层笔触渲染管线

这是一个纯本地、确定性的非真实感渲染管线。它把闭合矢量场景转换为轮廓线、排线阴影、色块涂抹三层笔触，并提供增量失效、内容寻址批次、原子版本提交、强杀恢复和版本原子预览。

实现不依赖 Docker、数据库或外部服务。运行时只使用 Python 标准库、NumPy 与 PySide6。

## 环境与运行

本次验证环境：

- Python `3.11.18`
- pip `26.3`
- PySide6 `6.12.0`
- NumPy `2.4.6`
- pytest `9.2.0`

从空环境复现：

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python pip
.venv/bin/python -m pip install --upgrade PySide6 numpy pytest
.venv/bin/python -m pip install -e .
```

执行测试与完整回归：

```bash
.venv/bin/pytest
.venv/bin/npr-pipeline regression --out artifacts/regression --seed 20261122
```

另有固定版本参考文件 `requirements.txt`。

## 入口

```bash
# 生成 3 类确定性样本与基线哈希
.venv/bin/npr-pipeline generate --out samples --seed 20261122

# 校验场景并输出确定性几何表示与分区索引
.venv/bin/npr-pipeline parse-scene \
  --scene samples/balanced/scene.json \
  --out samples/balanced/geometry.json

# 初次或增量渲染
.venv/bin/npr-pipeline render \
  --scene samples/balanced/scene.json \
  --params samples/balanced/params.json \
  --workdir runs/balanced \
  --trace traces/balanced.jsonl

# 从磁盘清单、哈希链与 staging 中恢复
.venv/bin/npr-pipeline recover --workdir runs/balanced

# 用同一参数继续未完成批次
.venv/bin/npr-pipeline render \
  --scene samples/balanced/scene.json \
  --params samples/balanced/params.json \
  --workdir runs/balanced \
  --trace traces/balanced.jsonl

# PySide6 固定周期预览窗口
.venv/bin/npr-pipeline ui \
  --scene samples/balanced/scene.json \
  --params samples/balanced/params.json \
  --workdir runs/balanced
```

## 场景格式

场景 JSON 使用 `npr-scene-v1`。每个分区引用一条闭合路径；分区之间允许共享边界，但不允许面积重叠。

```json
{
  "schema": "npr-scene-v1",
  "canvas": {"width": 960, "height": 640},
  "draw_order": ["zone_body"],
  "zones": [
    {
      "id": "zone_body",
      "label": "body",
      "fill": "#d9a06f",
      "path": "path_body"
    }
  ],
  "paths": [
    {
      "id": "path_body",
      "closed": true,
      "start": [20, 30],
      "segments": [
        {"type": "line", "to": [120, 30]},
        {"type": "quadratic", "control": [160, 70], "to": [120, 110]},
        {"type": "line", "to": [20, 110]},
        {"type": "line", "to": [20, 30]}
      ]
    }
  ]
}
```

校验规则：

- `schema`、画布、路径、分区、绘制顺序及必要字段必须存在。
- 所有坐标必须是有限数；路径必须是 `closed: true`，且最后一段终点回到起点。
- 每一条路径必须恰好被一个分区引用。
- 分区多边形使用确定性曲线细分构建；不同分区不得有正面积重叠。
- 任意失败返回 `E_SCENE_INVALID`，`location` 指向 `path:<id>`、`zone:<id>` 或字段位置。

`geometry_document()` 输出路径折线、弧长、包围盒、面积、`zone_index` 和 `geometry_hash`，可用于后续确定性计算。

## 三层笔触

参数 JSON 使用 `npr-params-v1`，三层参数分别位于 `outline`、`shadow`、`smear`，并共同包含 `grid_size`。

- 轮廓层：按路径弧长以 `spacing` 均匀采样，使用确定性 SplitMix64 抖动位置、方向和宽度。
- 阴影层：在分区内按 `angle_deg` 与 `spacing` 生成排线，根据实际几何净空修剪端点，并剔除与轮廓胶囊相交的笔触。
- 涂抹层：在分区填充范围内生成定向涂抹笔触，按局部阴影线长度密度降低不透明度。

同一场景与参数下，所有浮点数先量化，再看作为规范 JSON；批次文件使用内容哈希寻址，因此笔触顺序、几何和批次字节可重复。

## 依赖与增量失效

批次依赖因子：

| 层 | 依赖因子 |
|---|---|
| 轮廓 | `geometry.<zone>`、`outline`、`grid` |
| 阴影 | `geometry.<zone>`、`shadow`、`outline_geometry`、`grid` |
| 涂抹 | `geometry.<zone>`、`smear`、`shadow_density`、`outline_geometry`、`grid` |

失效规则：

- 轮廓颜色/透明度变化：只重算轮廓层。
- 轮廓宽度/疏密/抖动/方向/种子变化：轮廓层重算，并保守重算阴影剔除闭包与涂抹密度闭包。
- 阴影颜色/透明度变化：只重算阴影层。
- 阴影几何/间距/方向/抖动/种子变化：重算阴影层与涂抹层。
- 涂抹参数变化：只重算涂抹层。
- 单个分区几何或填充变化：该分区三层保守重算。
- 采样网格变化：全部层重算。

无法证明无影响时采用保守集合，并在轨迹 `reasons` 中记录 `outline_culling_closure`、`shadow_density_closure`、`dependency_version_changed_conservative` 等原因。

## 批次路由与提交

每层笔触按其中点落入统一采样网格，批次 ID 形如 `outline:zone_00:r1c2`，对应分块 `r1c2`。每个批次一个 `.nprb` 分块文件，内容为 magic + 规范化 JSON。

工作目录布局：

```text
workdir/
  CURRENT.json
  manifests/v000001.json
  manifests/v000002.json
  chunks/l0/<batch-key>-<content-hash>.nprb
  chunks/l1/...
  chunks/l2/...
  staging/state.json
  work.lock
```

提交协议：

1. 批次结果写入同目录临时文件，`fsync` 后 `os.replace`。
2. staging 清单原子更新已完成批次及真正待写批次。
3. 全部批次完成后构造新版本清单，包含版本号、前序清单哈希、依赖因子、批次/分块/内容哈希。
4. 先原子写 `manifests/vNNNNNN.json`，再原子替换 `CURRENT.json`。
5. 失败返回 `E_COMMIT_FAILED`，清理 staging 与未引用分块，旧 `CURRENT` 保持不变。

同一工作目录由 `flock` 排他锁保护；进程内同一场景另由 `RenderService` 防重，重复提交返回 `E_BUSY`。

## 恢复协议

`recover` 首先从 `CURRENT.json` 验证版本号、清单哈希、前序哈希链、每个分块存在性与 SHA-256。任一断点返回 `E_RECOVERY_INVALID`，并定位到 `CURRENT`、`manifests/...` 或 `batch:<id>`。

验证通过后：

- 保留最近完整已提交版本作为预览源。
- 检查 staging 的基版本、场景、参数、因子和已完成分块。
- 丢弃临时文件、无效 staging 与未被当前链引用的分块。
- 返回 `completed_batches` 与 `missing_batches`；再次以相同场景/参数执行 `render` 时，只写缺失批次，已完成 staging 批次直接复用。
- `recover --discard-staging` 可彻底丢弃所有未提交批次。

## 预览一致性

`PreviewService` 加载一次清单及其所有分块，构造不可变 `PreviewSnapshot`，在锁内整体替换引用。快照包含同一版本的层、批次、哈希和笔触集合，不会出现半更新状态。

UI 使用固定间隔 `QTimer` 刷新。旧快照通过 `resolve_handle()` 读取时返回 `E_STALE_HANDLE`。后台重算失败、取消或被强杀时，上一份快照继续可见。

PySide6 预览绘制按量化画笔样式聚合为 `drawLines` 批量调用，避免当前 PySide6 在单帧大量 `drawLine` 时触发的 C 扩展析构缺陷。

## 轨迹日志

轨迹为规范 JSONL，无时间戳与绝对路径。每个批次记录包含：

- `version`、`layer`、`batch`、`chunk`
- `affected_batches`、`reused_batches`、`chunk_count`
- `excluded_strokes`、`stroke_count`
- `content_hash`、`reasons`

每个版本还输出层汇总和全版本汇总。相同种子、样本与操作序列产生逐字节一致的样本、分块、清单与轨迹。

## 错误码

- `E_SCENE_INVALID`：场景字段、坐标、闭合、分区引用或重叠错误。
- `E_COMMIT_FAILED`：分块/清单写入失败，旧版本保持有效。
- `E_RECOVERY_INVALID`：清单版本、哈希链或分块校验失败。
- `E_STALE_HANDLE`：预览快照已被更新版本取代。
- `E_BUSY`：同一场景或工作目录已有重算任务。
- `E_CANCELLED`：任务在批次边界响应取消。

## 验证结果

`artifacts/regression/report.json` 是完整回归记录，包含 3 类样本、增量复用、注入写失败、真实 `os._exit(91)` 强杀、恢复续算、预览哈希和轨迹哈希。详细结果见 `docs/VALIDATION.md`。
