# diffrender

面向游戏与互动媒体美术资产的可微渲染、逆向成像、场景格式互操作和可恢复优化组件。

- Go 1.22+，仅标准库，无外网依赖。
- 三个场景 schema：`1.0`、`2.0`、`3.0`。
- 球体 / 平面 / 矩形光源；前向光线投射、软阴影近似、前向自动微分与光度损失反传。
- 原子、串行、带内容哈希的固定窗口检查点；中断后只从匹配快照恢复。
- 一次性全量、实时窗口推进、暂停后恢复三条路径逐位一致。
- 内置固定种子样本、二进制校验目标图、完整验收报告。

## 快速开始

```bash
go version
go test ./...
go vet ./...
go run ./cmd/diffrender
```

默认生成 3 个固定种子任务到 `artifacts/demo`，执行完整验收并写入
`artifacts/demo/report.json`。常用参数：

```bash
go run ./cmd/diffrender \
  -seed 20261124 \
  -tasks 3 \
  -out artifacts/demo \
  -iterations 64 \
  -window 4 \
  -workers 4
```

如果系统能找到 `go` 但找不到 `gofmt`，可直接使用：

```bash
"$(go env GOROOT)/bin/gofmt" -w .
```

## 目录

| 路径 | 说明 |
| --- | --- |
| `scene/` | 统一内部模型、1.0/2.0/3.0 加载器、升级轨迹、降级损失清单、确定性导出、往返比较 |
| `render/` | 光线投射、软阴影、前向 AD、目标图二进制格式、块并行批处理与重试 |
| `optim/` | 单调线搜索优化、窗口提交、快照验证/恢复、三路径比较与 `E_DIVERGE` 定位 |
| `sample/` | 固定种子任务、三版本场景、两相机目标图、解析真值 |
| `demo/` | 完整验收流程与报告 |
| `cmd/diffrender/` | 可运行命令行示例 |
| `artifacts/demo/` | 已生成样本、目标图、检查点、重建结果和验证报告 |
| `docs/FORMAT.md` | schema、目标图、检查点和错误格式说明 |

## 场景模型与互操作

内部模型统一为 `scene.Scene`，保留 `SourceVersion`，并始终以规范化的
`WorkingVersion = "3.0"` 表示。三种输入格式的差异：

| 维度 | 1.0 | 2.0 | 3.0 |
| --- | --- | --- | --- |
| 版本字段 | `schema_version` | `schema` | `schemaVersion` |
| 几何组织 | 扁平 `objects[]` | `geometry.spheres/planes` | `nodes.spheres/planes/areaLights` |
| 球半径 | `radius` | `diameter` | `radiusMeters` |
| 光源功率 | `radiant_power_w` (W) | `radiant_power_mw` (mW) | `luminousFluxLm` (lm, `/683`) |
| 相机外参 | `eye/look_at/up` | 4x4 `world_to_camera` | `positionXYZ + orientationQuat` |
| 颜色 | 数组 | `{r,g,b}` | 数组 |

低版本升级时补入声明默认值，并在 `Scene.Trace` 记录：

```json
{
  "field": "render.shadowSoftness",
  "oldValue": "<missing>",
  "defaultValue": 0.08,
  "defaultSource": "schema-v2-default",
  "fromVersion": "1.0",
  "toVersion": "2.0"
}
```

当前默认补齐：

- 1.0 -> 2.0：`render.shadowSoftness = 0.08`
- 1.0/2.0 -> 3.0：每个球/平面的 `material.clearCoat = 0`
- 2.0 -> 3.0：`render.toneMapping = "linear"`

加载错误 `E_VER` 同时携带文件与字段路径；未知版本、缺失顶层结构、缺失
必填几何/相机/光源字段都会定位到具体路径。

### 导出与损失报告

```go
result, err := scene.VerifyRoundTrip(model, scene.ExportOptions{
    Version: scene.Version1,
})
```

- 重复导出同一模型逐字节一致。
- 目标版本不能表达且值非默认时返回 `E_LOSSY`，`errs.Error.Details` 为
  `[]scene.LossItem`，每项包含 `field`、`originalValue`、`disposition`。
- `AllowLossy: true` 时显式忽略这些字段；几何中心、半径、平面参数和光源功率
  永远按目标版本单位无损映射。
- `CompareSemantics` 只忽略表示规范化差异（单位、矩阵、四元数、默认补齐），
  其余逐项比较；不一致返回带首个字段路径的 `E_ROUND`。

## 可微渲染

渲染参数顺序稳定为：

```text
sphere_main.center.x
sphere_main.center.y
sphere_main.center.z
sphere_main.radius
sphere_main.albedo.r
sphere_main.albedo.g
sphere_main.albedo.b
plane_ground.albedo.r
plane_ground.albedo.g
plane_ground.albedo.b
light_key.radiantPowerW
```

前向模型：

1. 由相机 `eye/look_at/up` 和垂直 FOV 生成每像素射线。
2. 与每个球求解析二次交点，与每个平面求解析交点，取最近正交点。
3. 球法线为 `normalize(point - center)`，平面法线为平面法线。
4. 每个矩形光源用中心采样，辐照近似为 `P / (4πd²)`，乘 Lambert 项。
5. 软阴影用球体至光线最近距离的 `smoothstep` 半影；半影宽度由
   `shadowSoftness` 与矩形光源尺寸共同决定。
6. 对 `center/radius/albedo/light power` 做前向 AD；损失为 RGB MSE。

`render.DecodeTarget` 在解析任何像素前验证 SHA-256；损坏目标返回
`E_DECODE`，字段通常为 `target.checksum`。

## 优化

`optim.Config` 提供：

- `LearningRate`
- `Iterations`
- `GradClip`
- `Tolerance`
- `WindowSize`
- `MaxBacktracks`
- `MinStep`
- `Render`（工作线程、块大小、队列、重试）
- `PauseAfterWindow`（执行控制，不进入配置哈希）
- `GradientHook`（测试/扩展钩子，不进入配置哈希）

更新采用分组预条件方向 + 回溯线搜索，只有候选损失不大于当前损失时才接受。
因此无异常时损失序列单调不增。非有限梯度或候选参数会在提交前中止并返回
`E_GRAD`；当前轮参数整体回滚，不污染其他参数或已提交窗口。

## 检查点

每次完成 `WindowSize` 轮后提交一个窗口，字段包括：

- 当前场景参数；
- 优化器状态：迭代、损失、步长、梯度范数、接受/拒绝次数；
- 本窗口各块梯度累积缓冲；
- 完整损失序列；
- 源格式版本、算法版本、任务哈希、配置哈希、内容哈希。

写入规则：

- 同目录临时文件 -> `fsync` -> `rename` -> 目录 `fsync`；
- 文件锁 + 进程内互斥，窗口只能按 `0..N` 顺序追加，禁止覆盖；
- 先写窗口文件，再以原子 manifest 作为提交点；
- 读取时验证 manifest、每个窗口序号、内容哈希、参数与场景一致性、源版本、
  算法版本、任务哈希、配置哈希；
- 任一不匹配则整个快照逻辑作废，Runner 清理后从第 0 轮全量重算。

返回给调用方的快照是深拷贝；修改返回值不会改写已提交窗口。

## 三条路径一致性

同一初始场景、目标图和配置下：

- `oneshot`：一次调用跑完；
- `live`：实时按窗口提交；
- `resume`：窗口 1 后暂停，重新加载持久化状态继续；
- 串行 `MaxWorkers=1` 与并行 `MaxWorkers=4` 也保持逐位一致。

`optim.CompareResults` 使用 `math.Float64bits` 比较最终参数、损失序列和窗口
提交序列。首个差异返回 `E_DIVERGE`，`Details` 包含：

```json
{
  "field": "sphere_main.center.x",
  "window": 0,
  "left": 0.123,
  "right": 0.124,
  "detail": "parameter mismatch"
}
```

## 并发与异常边界

- 硬上限：`MaxWorkers <= 64`、`BlockPixels <= 65536`、
  `QueueCapacity <= 65536`。
- 每个 worker 只持有 `BlockPixels * 3` 的 float64 scratch buffer。
- 块任务进入有界 channel；队列满时发送方阻塞，不丢块。
- 每个块独立记录错误和尝试次数；单个块返回错误或 panic 只标记该块失败。
- `Batch.RetryFailed` 只重算失败块，已完成块不重算、不污染，最终按固定块序
  聚合，因此并行与串行逐位一致。
- Context 取消发生在未提交窗口时，该窗口丢弃；重启只能读取上次 manifest
  已提交且版本匹配的快照。

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_VER` | 未知版本、缺失必填字段、schema 结构错误 |
| `E_LOSSY` | 降级目标无法表达非默认字段 |
| `E_PARA` | 参数有限性/范围/单位约束失败 |
| `E_DECODE` | 目标图校验和、长度、尺寸或像素损坏；优化边界非法目标 |
| `E_GRAD` | 梯度/候选参数非有限，事务回滚 |
| `E_ROUND` | 导出后重新加载语义不一致 |
| `E_DIVERGE` | 全量/实时/恢复/并行串行结果不一致 |
| `E_CANCEL` | 用户取消 |
| `E_CKPT` | 检查点序号、哈希、版本或内容不一致 |
| `E_RENDER` | 块失败且重试耗尽 |
| `E_CONFIG` | 并行度/缓冲/优化配置越界 |
| `E_IO` | 本地文件读写失败 |

所有业务错误使用 `errs.Error`，包含 `Code`、`File`、`Field`、`Message`，必要时
包含 `Details`。

## 实际验证结果

环境：Go `go1.22.12 linux/amd64`。

`go test ./...`、`go test -race ./...` 与 `go vet ./...` 均通过。核心测试覆盖：

- `demo` 端到端测试自动执行生成、加载、优化、暂停恢复、并行/串行、导出和异常码检查；
- 三版本统一加载、升级轨迹数量、缺失字段/未知版本定位；
- 三版本确定性导出、字节稳定、跨版本往返、`E_LOSSY`/`E_ROUND`；
- 前向结果与目标图逐位一致、AD 与中心差分梯度方向/数值一致；
- 串行/并行损失、梯度、图像逐位一致；
- 块错误与 panic 隔离、单块重试、硬资源上限；
- 目标校验和损坏 `E_DECODE`、参数越界 `E_PARA`；
- 损失单调收敛、三条优化路径逐位一致；
- 快照不可变、并发提交串行化、篡改后整体作废并全量重算；
- 非有限梯度事务回滚 `E_GRAD`、取消后仅恢复已提交窗口；
- `E_DIVERGE` 首个字段与窗口定位。

默认验收运行（3 个任务）结果：

| 任务 | 初始损失 | 最终损失 | 窗口数 | 中心误差 | 半径误差 | 反照率误差 | 光功率误差 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `task-20261124` | 1.5773e-4 | 7.5741e-13 | 7 | 1.24e-5 | 5.50e-6 | 2.66e-3 | 0.27% |
| `task-21261127` | 1.6033e-4 | 9.2585e-13 | 13 | 1.64e-5 | 3.27e-6 | 1.81e-3 | 0.23% |
| `task-22261130` | 1.9621e-4 | 9.6247e-13 | 10 | 1.27e-5 | 7.80e-6 | 8.86e-5 | 0.01% |

总体结果：

```text
allThreePathsEqual = true
allConverged       = true
allChecksPassed    = true
```

验收覆盖的错误码：

```text
E_DECODE E_VER E_PARA E_LOSSY E_ROUND E_GRAD E_DIVERGE E_CANCEL E_RENDER
```

逐任务窗口哈希、损失序列、导出 SHA-256 和错误详情见
`artifacts/demo/report.json` 与 `artifacts/demo/task-*/task-report.json`。

连续两次干净运行后，除空锁文件外全部产物逐字节一致；`report.json` 的
SHA-256 为：

```text
7dc536d659d6325f4dc7ebcb59a03713a26e8ed40ccee77c493aea2378cd0191
```

## 设计边界

- 矩形光源采用中心采样加尺寸相关半影，是面向可微逆问题的近似，不是 Monte
  Carlo 面光源积分。
- 平面为无限平面；场景至少包含一个球、一个平面、一个矩形光源和一台相机。
- 可微参数上限为 32 个，默认任务含 11 个。
- 多相机目标与场景相机按 ID/分辨率严格匹配。
- 本组件只处理本地确定性样本，不发起网络请求。
