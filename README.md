# 石窟华盖离线软光栅渲染器

这是一个不依赖外部图片、模型或网络的确定性离线渲染组件。场景、褶皱几何、流苏、油灯、龛壁与相机均由代码生成；渲染器读取 JSON，输出可逐像素核对的 RGBA 图、16 位织物归属图、逐层预乘 Alpha 图、稳定字段顺序的渲染报告和可恢复快照。

## 环境

- Python 3.11+
- NumPy
- Pillow

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
source .venv/bin/activate
python -m pip install -e .
```

本仓库实际验证使用 Python 3.11.18、NumPy 2.4.6、Pillow 13.0.0。

## 运行

```bash
# 1. 确定性生成完整验证场景
python -m canopy_render generate-scene \
  --out artifacts/canopy_render_validation/scene.json

# 2. 完整渲染；输出 render.png / ownership.png / fabric_composite.png /
#    layer_NNNN_premultiplied.png / render_report.json / snapshot.json
python -m canopy_render render \
  --scene artifacts/canopy_render_validation/scene.json \
  --out artifacts/canopy_render_validation/reference

# 3. 可处理中断：完成当前块、提交快照、写 recovery_report.json、退出码 0
python -m canopy_render render \
  --scene artifacts/canopy_render_validation/scene.json \
  --out artifacts/canopy_render_validation/resumed \
  --interrupt-after-tiles 4

# 4. 硬杀测试钩子：snapshot.json 提交后不可捕获退出
python -m canopy_render render \
  --scene artifacts/canopy_render_validation/scene.json \
  --out artifacts/canopy_render_validation/resumed \
  --hard-kill-after-tiles 4

# 5. 续算并同时与参考目录逐像素核对；不一致退出码 3
python -m canopy_render render \
  --scene artifacts/canopy_render_validation/scene.json \
  --out artifacts/canopy_render_validation/resumed \
  --reference artifacts/canopy_render_validation/reference

# 6. 独立对照命令
python -m canopy_render compare \
  --scene artifacts/canopy_render_validation/scene.json \
  --output artifacts/canopy_render_validation/resumed \
  --reference artifacts/canopy_render_validation/reference

# 7. 完整验收
python -m unittest discover -s tests -v

# 8. 一键生成本说明中的验证材料
python scripts/run_verification.py
```

## 场景模式

顶层字段：

- `render`：画布宽高、分块宽高、背景 RGBA、色调映射参数、归属图 alpha 阈值。
- `camera`：位置、目标、上向量、垂直视场角、近远裁剪面。
- `wall`：龛壁原点、单位法线、单位 `u/v` 轴、矩形半宽/半高、基础颜色。
- `layers[]`：`layer_index`、`thickness`、`transmittance`、`scatter_color`、`pattern_seed`、`folds[]`。
- `folds[]`：全局唯一 `fold_id`、顶点、UV、三角形索引。索引越界和退化三角形会被拒绝。
- `tassels[]`：`tassel_id` 与胶囊段 `a/b/radius`，用于遮挡与龛壁投影。
- `lights[]`：必须三盏；含位置、强度、灯盘半径、颜色、是否投影。
- `shadows`：每灯采样密度、确定性灯盘采样模式、软边扫描步长/范围、测量对象。
- `fabric_shading`：褶皱法线强度、最小余弦、透射参考厚度、alpha 与环境项。
- `validation`：几何 epsilon 和灯与织物的安全间距。

完整字段可直接查看 `generate-scene` 的输出。`canonical_scene()` 会规范化层、褶皱、三角形、流苏和灯的列表顺序；场景摘要只对规范化后的语义内容求 SHA-256。

## 关键实现

**确定性材质表**

每层只使用自己的非负整数 `pattern_seed` 生成 `64 x 64` 纹样表、法线 `u/v` 扰动表。PRNG 是显式 SplitMix64，不使用 `np.random`、Python `hash()` 或时间。纹样表经循环平滑后双线性采样；层列表重排不会改变表内容或像素。

**背光透射**

每个可见片元按相机朝向翻转法线，使用种子法线表扰动法线。透射使用厚度与入射角的 Beer–Lambert 路径模型：

```text
T_effective = T_material ** ((thickness / reference_thickness) /
                             max(abs(dot(normal, view)), min_cosine))
```

透射项和漫反射项都乘种子纹样；色调映射把高动态范围压到 `[0, white_cap]`，并保留褶皱深处非零纹样，不会出现整片 255 白。先在每像素选择该层最前片元，再严格按 `layer_index` 由内向外做预乘 Alpha 合成。

**流苏投影与软阴影**

灯盘采样点使用固定 Vogel 螺旋，采样顺序与输入灯列表顺序无关。对每个龛壁像素与灯采样点，计算“壁点→灯采样点”射线与所有流苏胶囊段的最短距离；距离小于半径即该采样被遮挡，可见率为未遮挡采样数/总采样数。这形成确定性的软边。

投影落点使用以下公式，场景与报告内均可复算：

```text
wall_point = light.position + t * (source_point - light.position)
t = dot(wall.origin - light.position, wall.normal) /
    dot(source_point - light.position, wall.normal)
```

报告包含源点、壁点、壁面 UV、射线参数、壁内标志、到壁距离；壁外点标记为 `outside_wall`，不参与遮挡。软边报告以指定的“单个流苏 + 单盏灯”做隔离扫描，避免其他灯和对侧流苏把宽度测量混合；归一化亮度为 `0.16 + 0.84 * visibility`。每条记录给出扫描样本的壁面 UV、可见率、亮度，以及 10%–90% 过渡宽度；最终图像仍使用全部灯与全部流苏。

**分块快照与恢复**

- 每完成一个块，先把完整快照写入同目录临时文件，`fsync` 后用 `os.replace` 整体替换。
- 快照包含完成块编号、场景摘要、每块 RGBA/归属/织物复合/逐层预乘图、颜色摘要和统计量；不保存可复用的块偏移。
- 恢复时重新从当前场景计算画布与块矩形；快照摘要、格式、画布、块布局、场景摘要、载荷摘要、长度和统计结构任一不符，整份快照丢弃并重算。
- 快照不依赖半成品 PNG；恢复后的所有输出由已提交块和剩余块的确定性结果组装。
- `SIGINT`/`SIGTERM` 只设置停止标志，当前块完成并提交后才写恢复报告并退出 0。

## 结构化错误

校验失败时仅写 `validation_errors.json`，不写任何图像。每个 issue 的稳定字段为：

```json
{
  "code": "MESH_INDEX_OUT_OF_RANGE",
  "layer_index": 1,
  "fold_id": 100,
  "light_id": null,
  "tassel_id": null,
  "message": "..."
}
```

主要业务码：

- `MESH_INDEX_OUT_OF_RANGE`
- `DEGENERATE_TRIANGLE`
- `TRANSMITTANCE_OUT_OF_RANGE`
- `DUPLICATE_LAYER_INDEX`
- `DUPLICATE_FOLD_ID`
- `LIGHT_INSIDE_FABRIC`
- `INVALID_SCENE`
- `RESUME_MISMATCH`

`LIGHT_INSIDE_FABRIC` 同时检测灯与有限厚度织物表面的距离，以及对闭合流形按实体角求绕数的体积包含。

## 退出码

- `0`：完整成功，或可处理中断已提交当前块和恢复报告
- `2`：场景校验拒绝
- `3`：参考对照不一致；同时写 `differences.json`
- `64`：命令行/场景文件读取错误
- `137`：仅用于测试的硬杀钩子

## 验收结果

`python -m unittest discover -s tests -v` 的 25 项测试全部通过。详见 `VERIFICATION.md` 和 `artifacts/canopy_render_validation/verification_summary.json`。

