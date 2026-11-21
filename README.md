# 离线体积光分块预览器

这是一个纯 CPU、无 GPU、无外部服务依赖的确定性体积光预览实现。它读取自定义二进制场景，完成相机光线求交、透明介质边界分段、自适应步进、体积散射、透射率与方向光阴影估计，并按屏幕分块执行有预算上限的并行调度。

实现同时覆盖预算降级、接缝验证、浮点/定点双路径、增量重渲、原子快照恢复、异常注入和 Tkinter 图形预览。

## 已实现能力

- 自定义单文件二进制场景格式，包含相机内外参、AABB/球体介质、方向光、密度场分块和 SHA-256 校验。
- 加载期校验矩阵可逆性、介质嵌套关系、密度索引/量/非有限值；业务错误定位到首个失败字段和介质标识。
- 声明式预算降级序列：`full -> balanced -> draft`，每级记录预检采样量、实际用量和步长。
- 分块任务并行，提交按 `seq` 稳定排序；同一分块内部像素/步进顺序固定。
- 整屏单块真值比对接缝，超差返回 `E_SEAM_UNRESOLVED`，包含像素坐标、两侧颜色和透射率。
- `float64` 与 Q20 定点累加双路径，逐像素颜色/透射率误差及保守误差上界。
- 相机、光源、介质、密度块变化的增量重渲；可证明未受影响的块复用旧内容哈希，否则保守重渲并记录原因。
- 每块哈希链快照，临时文件 + `os.replace` 原子写入；强制中止后从最近快照恢复，不重复写已完成块。
- 五类加载入口异常注入，各自返回独立业务码，且发生在写出任何颜色缓冲之前。
- Tkinter 预览器展示像素预览、分块网格、降级标签、预算占用曲线。

## 快速开始

项目环境要求 Python 3.11+、NumPy；GUI 需要 Tkinter。当前工作区使用带 Tkinter 的 Python 3.12 创建了 `.venv`：

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m pip install pytest
```

生成确定性样本并渲染双精度预览：

```bash
.venv/bin/python -m volrender.cli generate \
  --out artifacts/sample --sample-budget 12000

.venv/bin/python -m volrender.cli render \
  --scene artifacts/sample/scene.volbin \
  --config artifacts/sample/render_config.json \
  --out artifacts/preview
```

输出通过原子版本目录发布：

```text
artifacts/preview/
  CURRENT                       # 当前版本 ID
  current -> versions/<id>      # 原子切换的完整版本
  versions/<id>/
    color_float64.bin           # <f8, H x W x 3>
    transmittance_float64.bin   # <f8, H x W>
    color_fixed.bin
    transmittance_fixed.bin
    precision_abs_error.bin     # 颜色逐像素绝对误差
    precision_transmittance_abs_error.bin
    precision_error_bound.bin   # 逐像素保守误差上界
    preview.png
    preview_summary.json
    summary_float64.json
    summary_fixed.json
    manifest.json
```

运行测试和完整验收脚本：

```bash
.venv/bin/python -m pytest -q
.venv/bin/python scripts/verify_demo.py
```

验收报告写入 `artifacts/verification/report.json`，可目检预览位于 `artifacts/demo/preview/current/preview.png`。

## GUI

在具备桌面显示的机器上：

```bash
.venv/bin/python -m volrender.cli render \
  --scene artifacts/sample/scene.volbin \
  --config artifacts/sample/render_config.json \
  --out artifacts/gui-preview \
  --gui
```

界面包含像素预览、分块边框、降级阶段标签、逐块预算占用折线和阶段图例。无显示环境会返回 `E_DISPLAY_UNAVAILABLE`。

## 异常注入

注入发生在场景/配置加载入口，不修改渲染器或调度器：

```bash
.venv/bin/python -m volrender.cli render \
  --scene artifacts/sample/scene.volbin \
  --config artifacts/sample/render_config.json \
  --out artifacts/must-not-exist \
  --inject truncated_density
```

| 注入参数 | 业务码 |
|---|---|
| `corrupt_tile_index` | `E_TILE_INDEX_CORRUPT` |
| `missing_medium` | `E_MEDIUM_MISSING` |
| `nonfinite_camera` | `E_CAMERA_MATRIX_NONFINITE` |
| `truncated_density` | `E_DENSITY_TRUNCATED` |
| `budget_oversample` | `E_BUDGET_CONFIG_INVALID` |

普通场景校验失败统一返回 `E_SCENE_INVALID`，并带 `field`/`medium_id`。

## 快照恢复

```bash
# 模拟处理 2 个块后强制终止；只写快照，不写颜色输出
.venv/bin/python -m volrender.cli render \
  --scene artifacts/sample/scene.volbin \
  --config artifacts/sample/render_config.json \
  --out artifacts/recovery-preview \
  --mode float64 --snapshot-dir artifacts/snapshots --stop-after 2

# 从最近有效快照继续
.venv/bin/python -m volrender.cli render \
  --scene artifacts/sample/scene.volbin \
  --config artifacts/sample/render_config.json \
  --out artifacts/recovery-preview \
  --mode float64 --snapshot-dir artifacts/snapshots --resume
```

## 主要业务码

| 业务码 | 含义 |
|---|---|
| `E_SCENE_INVALID` | 场景字段、几何、嵌套或密度索引非法 |
| `E_BUDGET_CONFIG_INVALID` | 配置非法或注入超预算配置 |
| `E_BUDGET_EXCEEDED` | 所有声明降级阶段仍超过单块预算 |
| `E_SEAM_UNRESOLVED` | 分块结果超出整屏真值容差 |
| `E_RECOVERY_INVALID` | 快照版本、字段、哈希链或载荷不一致 |
| `E_BUSY` | 同一场景已有渲染任务在执行 |
| `E_INTERRUPTED` | 强制终止模拟或用户中断 |

## 文档

- `docs/FORMAT.md`：二进制容器、场景头、配置和确定性规则。
- `docs/DESIGN.md`：渲染、预算、接缝、精度、增量、发布、快照设计。
- `docs/VERIFICATION.md`：本次实际测试与验收数据。
