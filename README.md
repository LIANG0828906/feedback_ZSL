# VectorBool

本地矢量路径布尔运算组件，面向游戏 UI 素材编辑与离屏预览。支持直线、二次/三次贝塞尔、孔洞、自相交输入，以及并、交、差、异或四种运算。容差从入口解析一直传递到求交、选边、误差评估与光栅化，并在每个阶段留下结构化报告。

## 交付内容

- `src/vectorbool/parser.py`：JSON 路径解析、闭合补线、顶点吸附、轮廓方向规范化、定位到子轮廓/段。
- `src/vectorbool/intersect.py`：直线与贝塞尔分割求交、相切/近重合分类、容差变化后的交点差分。
- `src/vectorbool/boolean.py`：边排列、交点切分、环绕数选边、闭环拼接、孔洞方向、输出前自交检测。
- `src/vectorbool/evaluate.py`：自适应扫描线面积对称差、双向最近边界距离、容差放大比与降级判定。
- `src/vectorbool/injection.py`：五类入口异常注入与独立业务码。
- `src/vectorbool/preview.py`：统一采样网格、Qt 离屏 `QImage`、确定性 PNG 编码。
- `src/vectorbool/service.py`：后台线程、同一路径对互斥、取消与最后一次成功预览保留。
- `src/vectorbool/project.py`：版本/内容哈希校验、临时文件 + `fsync + os.replace` 原子保存。
- `src/vectorbool/generator.py`：确定性样本、基线、轨迹日志与回归入口。
- `src/vectorbool/ui.py`：PySide6 编辑器界面。
- `tests/`：41 项自动化测试。
- `scripts/run_acceptance.py`：一键验收并生成 `artifacts/acceptance_report.json`。

## 环境

实际验证环境：

- Python 3.11.18
- numpy 2.4.6
- PySide6 6.12.0
- pytest 9.2.0
- Ubuntu 26.04 / WSL2

系统未预装 pip，因此使用用户级 `uv` 安装独立 Python 3.11，不改动系统 Python：

```bash
# 当前 Ubuntu 环境同时受 PEP 668 管理且没有 ensurepip
curl -fsSL https://bootstrap.pypa.io/get-pip.py -o /tmp/get-pip.py
python3 /tmp/get-pip.py --user --break-system-packages
python3 -m pip install --user --break-system-packages uv

uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -e ".[test]"
```

Windows PowerShell 对应入口为 `.venv\Scripts\`。Qt 离屏环境可设置：

```powershell
$env:QT_QPA_PLATFORM = "offscreen"
```

该变量只用于无头测试/CI；运行编辑器时应保持未设置，让 Qt 使用当前桌面平台插件。当前只在 Linux/WSL2 实机验证；实现使用 `pathlib`、`os.replace`、Qt `QImage` 等跨平台接口，但未在 Windows 实机复验。

## 运行

```bash
# 重新生成确定性样本
.venv/bin/vectorbool generate --out artifacts/samples/samples.json

# 与既有基线比对；重新生成基线时可省略 --check-existing
.venv/bin/vectorbool regression --check-existing

# 单次布尔运算
.venv/bin/vectorbool run \
  --a path_a.json --b path_b.json \
  --operation union --tolerance 1e-6 \
  --preview preview.png --project saved.vbproj.json

# 段求交容差差分
.venv/bin/vectorbool intersections \
  --a segment_a.json --b segment_b.json \
  --old-tolerance 1e-6 --new-tolerance 1e-4

# 编辑器
.venv/bin/vectorbool ui --samples artifacts/samples/samples.json

# 测试与一键验收
QT_QPA_PLATFORM=offscreen .venv/bin/pytest -q
.venv/bin/python scripts/run_acceptance.py
```

## 路径 JSON

```json
{
  "fill_rule": "nonzero",
  "subpaths": [
    {
      "closed": true,
      "segments": [
        {"kind": "L", "points": [[0, 0], [3, 0]]},
        {"kind": "Q", "points": [[3, 0], [4.5, -0.75], [6, 0]]},
        {"kind": "C", "points": [[6, 0], [7, 1], [7, 2], [6, 3]]}
      ]
    }
  ]
}
```

- `L`：起点、终点。
- `Q`：起点、控制点、终点。
- `C`：起点、两个控制点、终点。
- `closed` 为真且末点未回到首点时，规范化阶段补闭合直线。
- 规范化输出的每个子轮廓包含 `vertices`、`segments`、`closed`、`contour_role`；段保留 `kind` 与全部点。

## 容差传播

1. 入口解析按容差吸附相邻端点、合并近重合顶点。
2. 贝塞尔按容差递归平坦化，容差同时用于交点参数边界与相切判定。
3. 边排列阶段按容差切分共线重叠、吸附交点、执行侧向探测。
4. 亚容差近平行边若无法稳定决定相交/分离，返回 `E_INTERSECT_AMBIGUOUS`，不静默丢弃。
5. 评估报告给出：
   - `area_symmetric_difference` 与 `area_relative_deviation`
   - `max_boundary_deviation` 与 `boundary_deviation_upper_bound`
   - `tolerance_amplification = boundary_upper_bound / tolerance`
   - `degradation_reasons` 和 `render_allowed`
6. 超过声明阈值时状态为 `degraded`，报告保留，但预览接口返回 `E_RESULT_DEGRADED`。

当前布尔输出使用容差内的确定性折线逼近贝塞尔；误差上的线性化上界为 `2 × tolerance`，并已计入边界偏差上界。

## 业务码

| 业务码 | 含义 |
| --- | --- |
| `E_PATH_INVALID` | 字段缺失、类型错误、段不连续等 |
| `E_NON_FINITE_COORDINATE` | 非有限坐标；同时带 `parent_code=E_PATH_INVALID` |
| `E_DEGENERATE_SEGMENT` | 段跨度不超过容差 |
| `E_ZERO_AREA_CONTOUR` | 轮廓没有实际填充面积 |
| `E_COINCIDENT_EDGE` | 注入的重合边路径对 |
| `E_HOLE_REVERSED` | 非零填充规则下孔方向反转 |
| `E_INTERSECT_AMBIGUOUS` | 交点落入无法稳定判定的容差带 |
| `E_SELF_INTERSECT` | 输出存在不可消除自交，并定位轮廓/段 |
| `E_RESULT_DEGRADED` | 误差超阈值，禁止进入渲染 |
| `E_BUSY` | 同一路径对已有后台任务 |
| `E_CANCELLED` | 后台任务取消 |
| `E_PROJECT_MISMATCH` | 工程版本或内容哈希不匹配 |
| `E_BOOLEAN_FAILED` | 未分类的内部运算失败 |
| `E_IO` | 离屏缓冲或文件 I/O 失败 |

## 确定性与并发

- 所有哈希输入使用排序键 JSON，不使用时间戳、文件大小或随机 UUID 参与内容哈希。
- 轨迹日志按 `(case, operation, tolerance)` 排序。
- 预览使用固定 512×512 采样网格、固定颜色和 Qt PNG 编码；相同轮廓与边界重复渲染的 PNG/RGBA 字节一致。
- 工程保存先写同目录临时文件，`fsync` 后 `os.replace`，再同步目录。
- 同一路径对的并发提交返回 `E_BUSY`；取消/异常不会覆盖最后一次成功预览。
- 工程只在结果与当前运算、容差匹配且误差报告通过时才标记为已提交；降级或未完成结果不会在载入时恢复为成功结果。

## 实际验证结果

```text
pytest: 41 passed
ruff: All checks passed
regression: checked=129, failures=0
trace: 116 ok, 8 E_INTERSECT_AMBIGUOUS, 5 injected entry rejections
preview byte identical: true
project roundtrip: true
```

`curved_partial_q`、容差 `1e-6` 的四运算结果：

| 运算 | 状态 | 交点数 | 轮廓数 | 面积相对偏差 | 最大边界偏差 | 上界 | 容差放大 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| union | ok | 1033 | 1 | 0.0 | 0.0 | 2e-6 | 2.0 |
| intersect | ok | 1033 | 1 | 0.0 | 0.0 | 2e-6 | 2.0 |
| difference | ok | 1033 | 1 | 0.0 | 0.0 | 2e-6 | 2.0 |
| xor | ok | 1033 | 2 | 0.0 | 0.0 | 2e-6 | 2.0 |

异常注入结果：

```text
coincident_edge       -> E_COINCIDENT_EDGE
degenerate_segment    -> E_DEGENERATE_SEGMENT
zero_area_contour     -> E_ZERO_AREA_CONTOUR
hole_reversed         -> E_HOLE_REVERSED
non_finite_coordinate -> E_NON_FINITE_COORDINATE
```

关键文件哈希：

```text
samples.json             bb3513b239b26bc8f1c88f204a243333fcdc6791d664ff8f526f5a4a44419122
baseline.json            76d63a38b4358575cda5676ee51e5a35a4a76f2600c37174b1e1d0b91b05405e
trace_log.json           1f6c8ec0a86629874d68719892cfee945791b3f563a90ca5884d29c26fc2f1f9
regression_report.json   64e7f2fd69f484b9d2009320cf510651c411c329fb7c90d454c26e2980848745
curved_union_preview.png 72248b252ee44a9e843490021d52ed10aa721d8e12a335664fadc0112a890b8b
acceptance_report.json   c6f1792f86282affaae6dd59fcdc922f3116144db0323538135856762a8b5342
```

验收产物：

- `artifacts/samples/samples.json`
- `artifacts/samples/baseline.json`
- `artifacts/samples/trace_log.json`
- `artifacts/samples/regression_report.json`
- `artifacts/samples/curved_union_preview.png`
- `artifacts/samples/acceptance_project.vbproj.json`
- `artifacts/acceptance_report.json`

