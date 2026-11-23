# 地形纹理几何变换与畸变校正组件 — 技术说明

## 1. 环境与运行

- 解释器：CPython 3.11.18（通过 `uv` 安装的独立构建，本地虚拟环境 `.venv`）。
- 依赖：`numpy 2.4.6`、`pillow 13.0.0`（见 `requirements.txt`）。
- 一键验证：`bash run_checks.sh`。
- 最小示例：`.venv/bin/python examples/run_demo.py`
  （`--interactive` 交互调参，`--gray` 单通道，`--workers N` 调并行度，
  `--order {0,1,2}` 切换最近邻/双线性/双三次）。

## 2. 模块结构

| 模块 | 职责 |
| --- | --- |
| `terrain_geo/geometry.py` | 单应、径向畸变多项式、仿射的解析正/反向映射与数值雅可比 |
| `terrain_geo/resample.py` | 反向映射重采样、三档插值、分块+重叠边带、确定性合并 |
| `terrain_geo/stages.py` | 三类阶段、参数范围校验、阶段缓存键推导、阶段执行 |
| `terrain_geo/pipeline.py` | 依赖拓扑调度、失效清单、线程池、原子缓存替换、全量比对 |
| `terrain_geo/persistence.py` | 原子快照写入/加载、整体作废校验、旧 blob 回收 |
| `terrain_geo/validation.py` | 控制点逐阶段重投影误差（中位数/上界）与 `E_GEOM` |
| `terrain_geo/samplegen.py` | 固定种子 `20261123` 的确定性地形样本与解析真值 |
| `terrain_geo/io_utils.py` | 图像读写，损坏文件映射为 `E_DECODE` |
| `terrain_geo/visualize.py` | 控制点叠加与差异热力图 |
| `examples/run_demo.py` | 端到端可运行示例（含异常链路与 REPL） |
| `tests/test_pipeline.py` | 24 项验收测试 |

## 3. 变换链与坐标约定

像素 `(i, j)` 中心坐标为 `(x=j, y=i)`。依赖链为线性 DAG：

1. `homography`：透视单应 `x' = H x`，反向映射用 `H^{-1}`。
2. `radial_correction`：前向为去畸变，模型
   `x_d = c + (1 + k1 s^2 + k2 s^4)(x - c)`，`s = |x-c|/radius`；
   重采样反向映射直接套用畸变多项式，前向控制点用 Newton 标量求逆。
3. `affine`：`x' = scale * x + translate`，输出尺寸由阶段参数推导。

通道独立：二维输入在内部升维为 `HxWx1`，输出恢复二维；三通道保持 3。

### 参数范围（越界即 `E_PARA`，带阶段名与字段名）

| 字段 | 范围 |
| --- | --- |
| `H` 元素绝对值 | `<= 1e6`，必须满秩、3x3、有限 |
| `output_size` 各维 | `1..16384` 整数 |
| `k1`, `k2` | `[-1.0, 1.0]` |
| `radius` | `[1.0, 1e5]` |
| `scale[*]` | `[1e-3, 1e3]` |
| `translate[*]` | `[-1e5, 1e5]` |

## 4. 缓存键与失效传播

阶段缓存键：

```
key = sha256(canonical_json({
    input_hash, upstream_key, stage_fingerprint
}))
stage_fingerprint = sha256({stage_id, param_version, params, resample})
```

- 键同时绑定输入内容哈希、上游键、阶段参数与参数版本，任一变化即换键。
- 调度按拓扑序计算键；键未命中只可能发生在被修改阶段及其下游，
  上游阶段直接命中内存或快照缓存。
- `PipelineResult.invalidation_listing()` 输出每阶段 `stage_id/state/reason`。
- 对外暴露的缓存值是拷贝，已提交结果无法被调用方静默改写。

## 5. 分块重采样与边界一致性

- 插值阶数：0 最近邻（半宽 0）、1 双线性（半宽 1）、2 Catmull-Rom 双三次（半宽 2）。
- 重叠宽度：在目标帧 16x16 确定性探针网格上计算反向映射数值雅可比，
  取各轴 `ceil(half_width / max|J|)` 的全局最大值，随畸变系数联动。
- `required_overlap > max_overlap` 或单块像素数超 `max_pixels_per_thread`
  时返回 `E_BAND`（含阶段名与字段）。
- 合并规则：块只负责其自然目标网格区域，重叠边带仅提供采样上下文，
  不做任何加权融合；分块与整图走同一个像素核，因此逐像素严格相等。
- 帧外采样统一补零，最近邻/双线性/双三次均做安全裁剪，无越界访问。

## 6. 增量调度、并发与一致性

- 线程池 `max_workers` 限定块级并行；缓存写入与快照提交在锁内串行，
  提交序列严格按阶段拓扑序。
- 每次运行返回本次实际重算的 `commit_sequence`，复用阶段不入列。
- `assert_matches_full()` 从空缓存冷启动全量重算，先比最终图像，
  再逐阶段比对并行/串行输出；首个不一致像素坐标与所属阶段通过 `E_DIFF` 返回。
- 单阶段异常包装为 `E_INTERNAL`（保留阶段名），不写入缓存、不影响其他阶段，
  重试时上游已提交结果继续复用。

## 7. 快照持久化与版本识别

存储布局：`work/snapshots/stage_<id>__<key>.npy` + `manifest.json`。

- 每个 blob 先写 `*.tmp` 再 `os.replace`，manifest 最后原子替换，全程持锁串行。
- manifest 记录每阶段 `cache_key/param_version/input_hash/upstream_key/output_hash`。
- 加载时从源哈希起逐级重算期望键并核对全部字段与 blob 内容哈希；
  任一不匹配（含 schema 版本、参数版本、blob 篡改、文件缺失）立即整体作废，
  绝不部分混用旧结果。
- 提交后自动回收未被当前 manifest 引用的历史 blob。

## 8. 错误码对照

| 码 | 触发场景 |
| --- | --- |
| `E_PARA` | 参数越界/矩阵奇异/尺寸非法，附 `stage` 与 `field` |
| `E_GEOM` | 控制点重投影误差超容差，附超差点编号、实测/期望坐标 |
| `E_BAND` | 重叠边带或线程像素缓冲上限不足 |
| `E_DIFF` | 增量与全量结果不一致，附首像素坐标与阶段 |
| `E_DECODE` | 图像缺失、截断或无法解码 |
| `E_INTERNAL` | 阶段内部失败，隔离且可重试 |
| `E_SNAPSHOT` | 快照清单损坏（预留） |

## 9. 实际验证结果

- `python -m unittest tests.test_pipeline`：24/24 通过（覆盖八项业务要求）。
- 示例端到端：冷启动三阶段提交 → 仅改 `k1` 后两阶段重算、首阶段复用 →
  增量与冷缓存全量结果逐像素相等 → 重启全量快照命中 →
  参数版本不匹配时三阶段全部重算 → 差异热力图输出。
- 几何校验：样本控制点在三阶段的重投影误差中位数与上界均为 0
  （解析真值与牛顿求逆残差低于 0.35px 容差）。
- 并行/串行：`workers=1` 与 `workers=4` 最终图像逐像素相等、提交序列相同；
  重复运行位级稳定。
- 灰度（单通道）与 RGB（三通道）两条链路均通过。

## 10. 确定性说明

样本生成器固定种子、无随机调度依赖；重采样核为逐元素确定性 NumPy 运算，
线程池只映射互不重叠的目标块，合并按固定网格归属，因此并行不引入
浮点求和顺序差异，输出可复现且位级一致。
