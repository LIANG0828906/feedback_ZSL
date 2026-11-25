# 技术说明与诊断手册

## 1. 确定性样本

生成器使用固定种子 `0x4D4F434150`（十进制 `332042289488`）和内置 xorshift RNG，不依赖 `math/rand` 的全局流。命令：

```bash
go run ./cmd/mocapbench generate -out samples
```

产物：

- `samples/manifest.json`：每个文件的 SHA-256、帧数、关节数。
- `samples/clips/*.json`：21 关节源骨架、48/60/36 帧、局部四元数、根平移、左右足接触布尔轨。
- `samples/targets/*.json`：18 关节高个额外脊柱目标、16 关节紧凑目标。
- `samples/truth/*.truth.json`：关键帧世界坐标、骨长、接触区间、根轨迹首尾和完整根轨迹。

加载时先校验 manifest SHA-256 和声明计数，再做结构校验。真值只用于独立的 `VerifyTruth` 与接触区间核对，不进入误差上界公式。

## 2. 错误码

| 代码 | 触发条件 | 定位字段 |
| --- | --- | --- |
| `E_SCHEMA` | 必需字段缺失、JSON 非法、偏移非有限值、父引用缺失、清单哈希不符 | `field` / `joint` |
| `E_CYCLE` | 父子引用成环 | `joint`、环路径 |
| `E_QUAT` | 四元数数量不符或数据非法 | `frame`、`joint`、`details.expected_index/first_extra_index` |
| `E_MAP` | 必需髋/足映射不存在或覆盖目标不存在 | `joint`、无法映射关节列表 |
| `E_SOLVE` | 迭代上限后角度/可达残余仍超容差 | `frame`、`joint`、残余 map |
| `E_CONTACT` | 支撑足漂移或接触端点帧偏差超限 | `frame`、`joint`、`details.drift_violations[].frame` |
| `E_STATE` | 非法帧状态迁移 | `frame`、`details.from/to` |
| `E_DIFF` | 三路径逐帧比对首个不一致 | `frame`、`joint`、`field`、A/B 值 |

## 3. FK 约定

关节局部变换采用 `T(offset) * R(q)` 约定：

```text
WorldRot[j] = WorldRot[parent] * LocalRot[j]
WorldPos[j] = WorldPos[parent] + WorldRot[parent] * Offset[j]
WorldPos[root] = RootTranslation + Offset[root]
```

因此关节自身旋转不改变自身位置，但会旋转所有子骨偏移；这与样本真值生成使用同一个 `ForwardKinematics` 实现。骨长为 `|Offset[j]|`。

## 4. 映射与比例归一

`BuildMapping(source, target, params)` 生成可查询对象：

- `BySource[name]`：源关节到一个或多个目标关节。
- `TargetSource[target]`：反向追溯。
- `UnmappedSource`：无目标的源关节，例如 `collar_l/r`、`toe_l/r`；紧凑目标还包括 `neck`。
- `DegradedSource`：显式 identity 降级关节。
- `ScaleRatios["source->target"] = |Offset_target| / |Offset_source|`。

默认 `spine -> [spine, spine2]` 是一对多扇出。全局比例使用 T-pose 高度比：

```text
G = Height_target / Height_source * GlobalScaleMultiplier
RetargetedRoot = G * SourceRoot
```

目标骨架保留自己的 T-pose 偏移，因此比例归一直接体现在目标骨长和根轨迹尺度上；单骨比例与全局 `G` 的差异用于定位肢体比例冲突。`Mapping.Hash` 与数值参数共同形成 `pv-xxxxxxxxxxxxxxxx` 参数版本。

## 5. 约束求解

每一帧从量化并映射后的局部旋转开始：

1. 对所有关节执行角度限位投影。限位包含绕指定轴的 signed angle interval 和总锥角上限。
2. FK 计算世界姿态。
3. 对手等末端的 `ReachLimit{Anchor, MaxDistance}` 做确定性 CCD 折叠。
4. 重新 FK 并计算角度/可达残余。
5. 直到 `maxAngle <= AngleTolerance` 且 `maxReach <= ReachTolerance`。

默认容差为 `1e-4`，最大迭代 24。生成样本实际最多 9 次（Hero）或 6 次（Compact）。若构造零角度限位和极小可达半径，投影会持续被限位拉回，最终返回 `E_SOLVE`，测试 `TestSolverReturnsESolveWithJointAndResidual` 覆盖该路径。

## 6. 接触校正与时间平滑

对每条足接触轨：

1. 找到当前连续接触区间。
2. 区间首帧用未校正 FK 足点建立 x/z 锚点，并令 `anchor.y = Skeleton.GroundY`。
3. 单足支撑时计算 `delta = anchor - foot_position`，多足时取平均最小二乘平移。
4. 使用因果指数平滑：

```text
smooth_t = (1-alpha) * smooth_{t-1} + alpha * target_delta_t
root_t   = raw_root_t + smooth_t
```

5. 平滑后再做一次接触残差平移投影，保证单足区间达到浮点精度锁定。
6. 重新 FK，记录每足实测漂移。

默认 `ContactDriftLimit=0.025`、`ContactFrameTolerance=1`。生成样本的最大漂移约 `2.5e-16`，接触区间端点偏差为 0。若把上界调到 `1e-18`，服务级测试验证返回 `E_CONTACT` 且错误含帧号。

恢复时，若重启点位于接触区间中部，从上一持久化帧的 `contacts.anchors` 与 `smoothed_correction` 重建因果状态，因此恢复路径与全量路径一致。

## 7. 误差传播上界

记：

- `δq = QuantStep`，默认 `1/1024`。
- `d_j`：目标骨架深度。
- `L_j = |Offset[j]|`。
- `r_j`：该关节映射骨长比；无显式比值时取全局 `G`。
- `I_f`：该帧求解迭代次数。
- `ε = 1e-14`。

量化组件舍入误差不超过 `δq/2`，归一化后使用保守角域系数 `8δq`。沿层级累积：

```text
QAng_j = QAng_parent + 8δq
QPos_j = QPos_parent + L_j * QAng_parent
```

比例归一的局部长度失配和累积上界：

```text
SLocal_j = L_source_j * |r_j - G|
SBound_j = SBound_parent + SLocal_j + ε max(1,L_target_j)
```

比例偏离还产生传播放大因子：

```text
A_j = (1 + |r_j-G|)^(d_j+1)
```

投影包络只使用量化步长、迭代次数、比例和深度：

```text
PAng_j = 16δq (I_f+1) A_j + ε(I_f+1)
PPos_j = PPos_parent + L_j * PAng_parent
```

FK 累积上界为量化位置上界、比例累积上界和浮点算术项之和；`TotalBound = FKBound + ProjectionBound`。

报告同时记录实际观测值：`quant_residual`、`scale_residual`、`fk_observed_error`、`angle_residual_radians`、`reach_residual`。这些值来自当前帧计算，不来自真值；上界公式也不读取 truth。前端“失效传播示意”按 `total_bound` 着色并显示深度。

## 8. 状态机、并发和检查点

合法迁移：

```text
pending -> solving
solving -> committed | failed
committed -> solving   # 参数变化后的增量/重算失效
failed -> solving      # 显式重试
```

其他迁移返回 `E_STATE` 且不修改原状态。

`BoundedPool[T]` 为每个 worker 建立容量 `BufferPerWorker` 的 channel。所有 worker buffer 已满时，提交者阻塞在发送上；没有 `select/default` 丢任务路径。测试 `TestBoundedPoolBlocksWhenBuffersFullWithoutDrop` 验证第三个任务会阻塞，worker 释放后不丢失。

SQLite：

- DSN 使用 `modernc.org/sqlite`，纯 Go。
- `SetMaxOpenConns(1)`。
- `Store.mu` 串行化写事务。
- 每帧一个 `BEGIN/INSERT OR REPLACE/COMMIT`，帧内容 SHA-256 与 `params_version`、`algo_version` 一起持久化。
- 重启时 `Initialize` 比较 meta 中的 clip/target/参数版本/算法版本；任一不匹配就在一个事务中 `DELETE FROM checkpoints` 并更新 meta。
- `LoadCommittedFrames` 按 `frame_index ASC` 读取连续前缀，反序列化后重算哈希校验。

## 9. 三路径一致性

`Service.Verify` 实际执行：

1. `full`：单线程顺序全量计算。
2. `realtime`：有界 worker pool 并行求解，接触校正按帧序执行，SQLite 按帧序串行提交。
3. `resume`：先持久化前 N/2 帧并返回模拟 `E_CRASH`，关闭 DB；重新打开、版本检查、读取连续前缀，再并行计算后缀。

`CompareResults` 比较：

- `root.x/y/z`
- 每个目标关节的 `rotation.w/x/y/z`
- `contacts.active`
- 所有 `FrameStats`
- 每个关节的全部误差残差/上界字段、深度和迭代数
- `content_hash`

首个差异返回 `E_DIFF` 的帧、关节和字段。由于所有路径调用相同的确定性纯函数，且恢复数据通过 JSON 往返（Go float64 shortest round-trip），样本上比较结果全部 OK。

## 10. Web 工作台

启动：

```bash
go run ./cmd/mocapbench serve -addr 127.0.0.1:8080
```

接口：

- `GET /api/state`：manifest、clips、targets、truth、默认参数。
- `POST /api/verify`：运行三路径验证；请求可带 `mapping_overrides`、`global_scale_multiplier`、`quant_step`、容差、worker/buffer。

页面无 CDN/外部资源，展示 SVG 骨架、帧播放、层级映射表、比例、误差曲线、容差倍数、接触时间轴、传播树、路径哈希和比对结果。修改映射 Targets 或全局比例后点击“应用映射并增量重算”，服务复用已加载样本并使新参数版本对应的检查点失效、重算受影响帧结果。

## 11. 实际验证结论

完整输出在 `docs/validation-results.txt`。摘要：

- Go：`go1.22.12 linux/amd64`。
- `go test ./... -count=1`：3 个包全部 `ok`。
- 样本真值：3/3 `truth_ok=true`，`world_max=0`。
- 6 个 clip/target 组合全部 `VERIFY OK`。
- 三路径：所有组合 `realtime_ok=true resume_ok=true`。
- SQLite：Python `sqlite3` 可读取 48 行连续检查点，meta 版本和首尾哈希正确。
- Windows：`CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 交叉编译成功。
