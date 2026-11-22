# 技术说明

## 1. 数据模型

场景 JSON 的一等对象：

- `wavelengths_nm`：波段列表，所有逐波长数组长度必须与它一致。
- `media[]`：`id`、`kind`、闭合 `boundary`、`ior[]`、`absorption[]`、`scattering[]`。
- `lights[]`：`directional_volume`、传播方向、`radiance[]`、`anisotropy[]`。
- `camera`：分辨率、垂直视场角、近远平面、严格递增时间的轨迹。

边界支持：

- `sphere`：`center` + 正 `radius`。
- `box`：`min < max` 的闭合 AABB。
- `mesh`：顶点/三角面，校验每条无向边恰好出现两次。

真值文件与场景通过 SHA-256 内容哈希绑定，避免错配。

## 2. 校验与错误模型

加载顺序为先 JSON 解码、再对象/字段结构、再几何、再参数。错误均抛出 `PipelineError(code, field, message, details)`。

| 场景 | 错误码 |
| --- | --- |
| JSON 损坏/文件不可解码 | `E_DECODE` |
| 缺字段、字段类型错误 | `E_SCHEMA` |
| 球半径非正、AABB 退化、网格开放边 | `E_GEOM` |
| IOR ≤ 1、负系数、非有限值、波段长度不符、时间不递增、步长/各向异性越界 | `E_PARA` |
| 非法分块迁移 | `E_STATE` |
| 输出不一致 | `E_DIVERGE` |

非法状态迁移在任何状态修改前完成检查，因此错误返回后分块保持原状态。

## 3. 界面折射与吸收

对每个像素和每个波段独立求最近介质边界交点。入射方向 `d`、外向法线 `n`、入射/出射折射率 `n1/n2`：

```text
cos_i = -dot(d, n)
eta   = n1 / n2
k     = 1 - eta^2 (1 - cos_i^2)
```

- `k < 0`：全反射，`d_reflect = d - 2 dot(d,n) n`。
- 否则：`d_refract = eta d + (eta cos_i - sqrt(k)) n`。

内部路径从入射点开始，对同一闭合介质继续求交。若出射界面发生全反射，方向反射后继续在介质内追踪，最多 `max_segments` 段。出射点真值取内部边界交点（出射折射之前）。

透射率：

```text
sigma_t(lambda) = absorption(lambda) + scattering(lambda)
T(lambda)       = exp(-optical_depth_quantized)
```

## 4. 固定点量化

```text
length_scale  = 1_000_000_000
optical_scale = 1_000_000_000_000
path_units    = round_half_up(path_length * length_scale)
optical_units = round_half_up(optical_depth * optical_scale)
```

`FixedAccumulator` 按段累加，并同时记录原始浮点值，输出：

- `path_units` / `optical_units`
- `path_length_quantized` / `optical_depth_quantized`
- `path_quantization_error` / `optical_quantization_error`

定点整数是快照和一致性比对的一部分，同一射线重复计算要求整数和浮点结果逐位一致。

## 5. 体积光单次散射

对每个内部路径段，从相机进入介质后的累计光学深度 `base_optical_depth` 出发：

```text
S_lambda = integral_0^L T(base + sigma_t s) sigma_s p(g, cos_theta) L_light ds
```

Henyey-Greenstein 相位函数：

```text
p(g, mu) = (1 - g^2) / (4 pi (1 + g^2 - 2 g mu)^(3/2))
```

`mu` 是视线方向与入射辐亮度方向（光源传播方向的反向）的夹角余弦。

数值积分支持：

- `midpoint`
- `left`
- `right`
- `trapezoid`

步数 `ceil(L / step_size)`，最后一段自动缩短。均匀介质单段解析参考为：

```text
S = L_light sigma_s p(g, mu) (1 - exp(-sigma_t L)) / sigma_t
```

若 `sigma_t == 0`，退化为 `L_light sigma_s p L`。

## 6. 分块状态机

状态：

1. `pending_intersect` / 待求交
2. `transmission_accumulating` / 透射率累积中
3. `scattering_accumulating` / 散射积分中
4. `committed` / 已提交
5. `failed` / 失败

合法边：

```text
pending -> transmission
pending -> failed
transmission -> scattering
transmission -> failed
scattering -> committed
scattering -> failed
committed -> pending        (仅显式 invalidate)
failed -> pending           (retry)
```

提交时记录该块的介质/光源版本和缓冲校验和。已提交块在未失效前不重新计算。

## 7. 依赖与增量失效

每个成功分块记录：

- 实际命中的介质 ID。
- 实际参与散射的逐波长体积光 ID。
- 字段级依赖：介质 `boundary/ior/absorption/scattering`，光源 `direction/radiance/anisotropy`。
- 提交时的介质/光源版本快照。

编辑介质或光源时：

1. 深拷贝并校验候选场景。
2. 通过后才提升对应资源版本。
3. 找到直接依赖该资源的分块。
4. 若边界/介质种类变化，额外把旧、新包围盒投影覆盖的分块作为保守候选。
5. 仅将这些块回到 `pending_intersect`；介质编辑清空透射与散射，光源编辑保留透射/路径/命中缓冲、只清散射。
6. 未命中的分块保留已提交状态和持久化缓冲。
7. 生成清单：块编号、资源种类/ID、字段、阶段、原因。
8. 立即持久化带新哈希/版本的增量快照。

因此玻璃球编辑在样本中仅失效 2/9 块，而不是全屏重建。

## 8. 检查点协议

`CheckpointStore` 使用：

- 进程内 `RLock`。
- Unix 下 `fcntl.flock` 文件锁；不可用时保留进程内串行写。
- 临时 `.tmp` 文件写入完整 NPZ。
- `flush + fsync` 文件，`os.replace` 原子替换。
- 对目录执行 `fsync`。
- 每个成功块附加一条 `commit_log.jsonl` 记录。

NPZ 快照包含：

- 完整透射/散射/路径单位/消光单位/命中掩码。
- 已提交掩码。
- 场景哈希、介质版本、光源版本、算法版本、配置哈希。
- 实际步长、预览误差上界。
- 逐块记录和稳定提交序列。

同一目录的 `resource_versions.json` 侧车保存当前场景哈希对应的介质/光源版本；重新加载已编辑场景时恢复版本号，避免把同内容新进程误判为版本不一致。若侧车与快照任一不匹配，仍按整快照作废处理。

兼容性检查项：

```text
scene_hash
media_versions
light_versions
algorithm_version
config_hash
actual_step
render_layout
committed_mask shape
每个已提交块的缓冲 checksum
```

任一失败都调用 `discard`，旧快照移动到 `rejected/`，然后从零全量重算，禁止把旧块和新块混合。

## 9. 并发与资源预算

- `ThreadPoolExecutor` 最大线程数 = `max_parallelism`。
- 待提交窗口 = `min(max_parallelism, max_pending_blocks)`。
- 求交缓存预算由 `BoundedSemaphore(intersection_cache_budget)` 控制。
- 实际介质求交使用有界 LRU（`BoundedIntersectionCache`），同一射线的多个波段复用最近边界交点，并记录 hits/misses/evictions。
- 每线程采样缓冲上限为 `per_thread_sample_buffer`；线程池总容量按并行度扩展。
- 租约按像素块申请，预算不足时阻塞等待，不丢块。
- 工作线程只计算并推进到 `scattering_accumulating`；`committed` 与检查点写入由调度线程按稳定分块顺序执行。

因此检查点写入不会相互交错，崩溃快照只包含已经提交的前缀。

## 10. 三路径一致性与差异定位

三条路径共享同一个 `render_block`，差异只在调度/恢复：

- `full`：从空状态顺序/并行计算并提交。
- `preview`：同样调度；默认使用同一步长。显式降低 `preview_step_size` 时记录实际步长和误差上界。
- `resume`：制造一次 `SimulatedCrash`，从最后一个原子快照加载，再补算剩余块。

`compare_results` 使用 `np.array_equal` 精确比较：

- 透射率
- 散射
- 路径固定点
- 消光固定点
- 逐波长统计（规范 JSON 精确值）
- 提交序列

发现差异时返回：

```json
{
  "code": "E_DIVERGE",
  "field": "scattering",
  "details": {
    "pixel": [17, 13],
    "block_id": 4,
    "wavelength_index": 1,
    "actual": 0.1,
    "expected": 0.099999
  }
}
```

## 11. 诊断界面

`tvol serve` 启动仅绑定本地的 `ThreadingHTTPServer`，无外部服务：

- `GET /api/state`
- `POST /api/render`
- `POST /api/compare`
- `POST /api/export`
- `GET /api/export.json`

面板展示：

- 相机投影后的介质边界叠加。
- 透射率热力图。
- 分块状态矩阵。
- 失效传播清单。
- 与一次新全量重算比较的差异热力图。
- 介质吸收/散射倍率、体积光强度倍率、步长编辑。
- JSON/NPZ/静态 HTML 导出。

`export-ui` 生成的静态 HTML 内嵌初始状态，可直接离线打开看到热力图、边界和状态矩阵。

## 12. 实际验收记录

命令：

```bash
.venv/bin/python scripts/run_validation.py
```

报告：`artifacts/validation_report.json`。

关键结果：

| 项目 | 结果 |
| --- | --- |
| 边界出射点最大误差 | `4.096204861636688e-15` |
| 路径长度最大量化误差 | `4.798459407595601e-10` |
| 透射率最大误差 | `4.4220183070819985e-13` |
| 散射积分最大误差 | `2.3493414999867235e-09` |
| 步长 `0.2 -> 0.0125` 收敛 | 单调下降 |
| 全量 vs 预览 | 全部检查逐位一致 |
| 全量 vs 崩溃恢复 | 全部检查逐位一致 |
| 崩溃点 | 第 4/9 块提交后 |
| 增量失效 | 玻璃球编辑仅 2/9 块 |
| 增量 vs 新全量 | 逐位一致 |
| 降低预览步长 | 实际 `0.025`，误差上界 `3.125835132575948e-07` |
| pytest | 42 项通过 |
| OpenGL | PyOpenGL 可导入，无窗口上下文，权威路径为软件光栅 |

## 13. 设计边界

- 单次散射，不含多次散射/全局光照。
- 介质均匀且使用独立闭合边界，不支持互相嵌套的重叠介质。
- 体积光源为方向光，不计算光源到散射点的阴影透射率。
- 预览降步长与全量不是严格逐像素等价；报告明确记录实际步长与误差上界，只有相同有效步长才参与严格三路径一致性验收。
- 操作系统强制终止进程时只保证已提交快照，不能承诺完整帧。

