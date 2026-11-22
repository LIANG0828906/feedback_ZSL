# GameEditor.Spatial

面向游戏编辑器与互动媒体场景的交互拾取、层级空间索引、增量失效与枢轴操作组件库。仓库同时包含确定性场景生成器、控制台验收工具、可运行的 Windows Forms 示例，以及不依赖第三方测试框架的自动验收套件。

全部实现仅使用 .NET 8 BCL 与 `System.Numerics`，不引用第三方 NuGet 包，不依赖网络、外部账号、GPU 或容器。

## 交付结构

| 路径 | 内容 |
| --- | --- |
| `src/GameEditor.Spatial` | 领域组件库：模型、校验、空间索引、拾取、增量失效、枢轴操作、快照、等价比对 |
| `src/GameEditor.Tools` | 控制台生成器、demo 与离线验收入口 |
| `src/GameEditor.WinForms` | 可交互 Windows Forms 示例 |
| `tests/GameEditor.Tests` | 18 项自动验收测试，纯控制台测试运行器 |
| `samples/default` | 固定种子生成的场景、查询、真值、编辑序列与编辑真值 |
| `scripts/verify.ps1` / `scripts/verify.sh` | 一键生成、构建、测试、快照往返验证 |
| `docs/ARCHITECTURE.md` | 索引、版本、依赖图、操作与并发设计说明 |
| `VERIFICATION.md` | 本机实际执行结果 |

## 快速开始

Windows PowerShell：

```powershell
dotnet build GameEditor.sln -c Release
.\scripts\verify.ps1
.\scripts\run-winforms.ps1
```

Linux / WSL（WinForms 可交叉编译为 `net8.0-windows`，但需在 Windows 上显示窗口）：

```bash
chmod +x scripts/verify.sh
./scripts/verify.sh
```

分步命令：

```bash
dotnet run --project src/GameEditor.Tools -- generate
dotnet run --project tests/GameEditor.Tests
dotnet run --project src/GameEditor.Tools -- demo
dotnet run --project src/GameEditor.WinForms
```

所有生成与持久化路径均为工作区内相对路径：`samples/default`、`artifacts/`。

## 确定性样本

`DeterministicSceneGenerator` 使用固定种子 `424242`、自实现 xorshift 随机数和固定写入顺序，生成：

- `scene.json`：18 个层级节点、14 个几何体（AABB、球体、三角网格），包含局部矩阵与声明哈希。
- `queries.json`：16 条屏幕射线、5 个框选矩形。
- `ground-truth.json`：每条射线的最近命中和逐几何体命中清单，以及框选节点集合。
- `edits.json`：局部变换、几何体增删、平移/旋转/缩放枢轴操作序列。
- `edit-truth.json`：每步内容哈希、节点世界矩阵与包围体、最终查询真值。
- `manifest.json`：样本文件 SHA-256 清单。

重复执行生成器得到逐字节相同的文件；测试会校验生成结果与仓库内样本一致。

## 核心 API

```csharp
var scene = SceneJsonCodec.LoadScene("samples/default/scene.json").Value!;
var queries = SceneJsonCodec.LoadQueries("samples/default/queries.json", scene).Value!;
var runtime = SpatialRuntime.Create(scene).Value!;

var ray = runtime.Raycast(queries.Rays[0]);          // 未命中时 Success=true, Value=null
var box = runtime.SelectBox(queries.Boxes[0]);       // 完整命中的节点集合
var frustum = runtime.SelectFrustum();

var updated = runtime.ApplyTransformBatch(new Dictionary<string, Matrix4x4>
{
    ["left-mesh-tip"] = newLocalMatrix
});
// updated.Value.Stats 可观察复用/重建数量
// updated.Value.Invalidations 给出精确失效对象与原因

var session = runtime.BeginPivotSession(selectedNodeIds, worldPivot).Value!;
session.Apply(PivotOperation.Translate(new Vector3(0, 1, 0)));
session.Commit();                                     // 原子批次提交

var full = runtime.CreateFullRebuild().Value!;
var report = EquivalenceChecker.Compare(runtime, full, queries.Rays, queries.Boxes);
if (!report.IsEqual)
{
    Console.WriteLine(report.Error);                  // E_DIVERGE + 首个对象/字段
    // report.IncrementalSnapshot / report.FullRebuildSnapshot 保留两侧快照
}
```

## 查询语义

- 射线：屏幕像素中心生成世界射线，返回距离最近的交点及所属节点/几何体；距离相同时按 `geometryId`、`nodeId` 稳定排序。未命中返回成功且值为 `null`。
- 框选：将几何体世界 AABB 投影到屏幕，与矩形相交即命中；返回去重且按节点 ID 排序的完整集合。
- 视锥：用相机近/远、左右、上下六个平面与世界 AABB 做保守包含测试。
- 查询不写任何历史状态；重复、逆序、并行执行结果一致。

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、类型错误、矩阵非有限、声明哈希/计数不一致 |
| `E_CYCLE` | 节点父子引用成环 |
| `E_REF` | 父节点、几何体宿主、网格索引等引用越界 |
| `E_TOPO` | 父子关系变化无法确定缓存归属，状态保持不变 |
| `E_STALE` | 版本冲突、未初始化或派生数据过期 |
| `E_SNAP` | 快照缺失、损坏、内容哈希/参数版本/结构版本不匹配 |
| `E_DIVERGE` | 增量路径与全量重建首次不一致 |
| `E_CANCELED` | 操作被取消，未提交状态不改变 |
| `E_LIMIT` | 查询批次或候选缓冲超过配置上限 |

## 增量更新保证

- 节点变换只重算自身与全部后代的世界矩阵、包围体、几何索引条目；祖先只更新聚合索引摘要；兄弟与非后代缓存对象不重建。
- 统计中 `ReusedNodes/ReusedEntries/ReusedIndexNodes` 表示按引用复用的缓存数量。
- 每个节点有 `TransformVersion`，条目记录其 `NodeTransformVersion`；查询前校验版本一致性，快照加载时版本不一致整体拒绝。
- 依赖图显式记录 `input:transform:*`、`input:geometry:*`、`derived:world:*`、`derived:bounds:*`、`derived:entry:*`、`index:node:*` 之间的边，并输出含对象 ID、原因与来源的失效清单。
- 任何失败批次使用写时复制提交：只有全部校验与派生更新成功后才交换状态；失败或取消保持提交前状态。

## 枢轴操作规则

- 选中集合先缩减为“最顶层选中节点”：若父与子同时选中，只对父执行，子通过父子传播移动，避免双倍位移。
- 平移把世界位移变换到根的父空间后提交。
- 旋转/缩放先在世界空间构造 `T(-w) * R/S * T(w)`，再共轭到根的父空间后右乘局部矩阵；枢轴点在操作后保持不动。
- 会话基于打开时的 `Revision`；提交时若场景已被其他编辑推进，返回 `E_STALE`，不会静默覆盖已提交结果。

## 快照

- 单个 JSON 文件整体包含派生节点、索引条目、层级摘要、内容哈希、结构哈希/版本、参数版本与快照哈希。
- 写入使用进程内信号量 + 文件锁串行化，同目录临时文件 `WriteThrough` 落盘后原子替换；失败不会暴露半写快照。
- `LoadInto` 校验失败返回 `E_SNAP` 且运行时保持空壳；`LoadOrRebuild` 会显式丢弃坏快照并全量重建。
- `LoadInto` 要求待加载场景与快照内容哈希/结构版本一致；编辑后的场景可通过 `runtime.ExportSceneDocument()` 持久化后重启加载。

## 验收

`dotnet run --project tests/GameEditor.Tests` 覆盖：

射线/框选真值与未命中、查询顺序无关、视锥、父子传播复用、精确失效、拓扑拒绝、批量回滚、父子同时选中不双倍、旋转/缩放枢轴不动、会话隔离/冲突、编辑序列与解析真值一致、增量与全量逐项一致、分歧定位、快照往返/失效/截断/并发原子写、单条目失败隔离、并行查询、取消边界。

详见 `VERIFICATION.md`。
