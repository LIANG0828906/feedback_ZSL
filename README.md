# 三维网格修复间：并发编辑、边界协商与快照恢复

本项目是一个 .NET 8 / OpenTK 4.9.4 的本地可运行实现，覆盖三维扫描三角网格的二进制读写、空间分块、跨块边界契约、多修复师并发发布、回退兼容性、拼接校验、原子快照恢复、无头确定性回归与窗口/离屏预览。

## 实际验证结果

本次在本地 Linux + .NET SDK 8.0.428 环境完成以下验证：

| 项目 | 结果 |
| --- | --- |
| `dotnet build MeshRepair.sln` | 成功，0 warning / 0 error |
| `dotnet test MeshRepair.sln` | 19/19 通过 |
| 基线样本 | 29 顶点、31 面片，含跨块孔洞、非流形边、退化面片、重复顶点和孤立碎片 |
| 最终发布版本 | v9，28 顶点、33 面片，2 个补齐面片，焊接减少 1 个顶点 |
| 拼接校验 | 通过，5 条边界边，0 裂缝/翻叠，法向偏差 0° |
| 快照恢复 | 从 v9 恢复，忽略未改名的临时文件，未提交块重算数为 0 |
| 确定性 | 两次运行的 `trace.jsonl`、`versions.json` 和全部快照逐字节一致 |
| 离屏预览 | 生成 `960x560` PPM，分块着色并绘制边界线 |
| OpenGL 窗口 | OpenTK 窗口编译通过；本地 `DISPLAY=:0` 下 `--window` 冒烟运行 4 秒未退出（环境有 EGL 驱动警告） |

验收产物：

- `artifacts/baseline/scan.mesh`：确定性二进制样本。
- `artifacts/baseline/sample.json`：样本顶点/面片信息、孔洞顶点、焊接顶点、非流形边、退化面、基线边界清单。
- `artifacts/baseline/baseline-trace.jsonl`：基线发布轨迹。
- `artifacts/acceptance/trace.jsonl`：完整并发、拒绝、回退、恢复轨迹。
- `artifacts/acceptance/versions.json`：每个发布版本的编辑者、块、边界清单版本/哈希、内容哈希和块哈希。
- `artifacts/acceptance/summary.json`：验收布尔项与统计。
- `artifacts/acceptance/determinism.json`：两次运行逐字节一致性。
- `artifacts/acceptance/snapshots/`：v1-v9 原子快照及一个模拟强杀残留的 `.tmp` 文件。
- `artifacts/acceptance/preview.ppm` / `preview.json`：无头离屏预览。

基线哈希：

```text
contentHash=9b281183c15d0728b3d5d8e1e3e83e924e144dbb20a393ab3e2af96c55ce6e18
fileHash=b7bb33ec84e3c5ce9c027b127793275ae52e5d5e716585c555ecadca4cf57e47
```

最终版本哈希：

```text
version=9
contentHash=a00f9f2839832923ac066f78a8c0ab49e9855fce72936700d9094a842b978116
traceSha256=155ad42d7bcc0ff4343b61305f0478f70e73b6644b6cfcede6100fcb716bacca
versionsSha256=1f8b0b692a4413f7e623e4073bec8e2bb2a927eb1d9104d0d332d549dc8f0e00
```

## 构建与运行

环境要求：

- .NET 8 SDK。
- 可访问 NuGet 以还原 `OpenTK 4.9.4`。
- 不依赖 Docker、数据库、外部服务或网络目标。

```bash
dotnet build MeshRepair.sln
dotnet test MeshRepair.sln
```

生成基线样本：

```bash
dotnet run --project src/MeshRepair.Cli -- sample \
  --out artifacts/baseline \
  --seed 1337
```

执行完整脚本化验收：

```bash
dotnet run --project src/MeshRepair.Cli -- verify \
  --out artifacts/acceptance \
  --seed 1337
```

`verify` 会执行两遍完整工作流并比较轨迹、版本日志和快照文件；任何字节差异会使命令以非零码退出。

也可以直接运行完整回归脚本：

```bash
./scripts/run-acceptance.sh
```

从快照恢复：

```bash
dotnet run --project src/MeshRepair.Cli -- recover \
  --snapshots artifacts/acceptance/snapshots
```

离屏渲染（无图形设备也可运行）：

```bash
dotnet run --project src/MeshRepair.Preview -- \
  --snapshots artifacts/acceptance/snapshots \
  --headless \
  --out artifacts/acceptance/preview.ppm
```

OpenGL 窗口渲染：

```bash
dotnet run --project src/MeshRepair.Preview -- \
  --snapshots artifacts/acceptance/snapshots \
  --window
```

窗口渲染线程每帧通过 `MeshRepairRoom.AcquireSnapshot()` 获取最近完整发布版本，构建分块着色三角形与橙色边界线；不会读取编辑会话的暂存数据结构。

## 解决方案结构

```text
src/MeshRepair.Core/
  Binary/       自定义 MRM1 二进制读写、数据长度与索引校验、内容哈希
  Model/        网格、拓扑、基线法向、统计和几何工具
  Chunks/       空间分块、唯一面片归属、边界清单、孔洞环发现
  Versioning/   不可变发布版本、版本组装、快照句柄、编辑房间
  Editing/      块租约、暂存编辑、焊接/补洞/碎片删除/法向重算
  Splice/       拼接守恒、边界双边引用、裂缝、翻叠和重复面检测
  Recovery/     快照 JSON、自哈希、版本链、原子保存和最近有效版本恢复
  Sample/       确定性跨块样本生成器
src/MeshRepair.Cli/
  sample/verify/recover 命令、轨迹与确定性检查
src/MeshRepair.Preview/
  OpenTK 窗口渲染与软件离屏 PPM 渲染
tests/MeshRepair.Tests/
  19 个 xUnit 回归测试
```

## 二进制格式 MRM1

固定小端布局：

| 偏移 | 宽度 | 字段 |
| --- | --- | --- |
| 0 | 4 | Magic `MRM1` |
| 4 | 1 | 坐标类型：`1=Float32`、`2=Float64` |
| 5 | 1 | 索引宽度：`2=UInt16`、`4=UInt32` |
| 6 | 2 | 保留标志，写 0 |
| 8 | 4 | 顶点数 `uint32` |
| 12 | 4 | 面片数 `uint32` |
| 16 | 8 | 网格版本号 `uint64`，必须非零 |
| 24 | N | XYZ 顶点坐标，按坐标类型连续存储 |
| ... | M | 三角形三个索引，按索引宽度连续存储 |

读取规则：

- 头部缺失、Magic 错误、类型非法、版本为 0、数据长度不符、索引越界、非有限坐标都会返回 `E_MESH_INVALID`。
- `Field` 指向首个异常字段，例如 `Version`、`FaceCount`、`DataLength`、`Faces[0].Index[0]`、`Vertices[17]`。
- `FaceCount` 声明超出实际面片载荷时定位到 `FaceCount`；多余载荷定位到 `DataLength`。
- `ContentHash` 对“类型 + 数量 + 坐标载荷 + 面片载荷”做 SHA-256，不包含版本号和 Magic；`FileHash` 对完整文件做 SHA-256。
- 写出时沿用同一头部布局，支持临时文件加原子重命名。
- 非流形边和退化面是扫描修复输入中的合法诊断项，不会在载入阶段被拒绝。

## 分块与边界契约

- `ChunkLayout` 支持 X/Y/Z 可配置网格划分；样本使用 `2 x 1 x 1`。
- 面片按质心落到唯一块；同时记录该面 AABB 覆盖的候选块和 `Basis`。跨多块面片仍只有一个 Owner。
- 相邻占用块生成 `BoundaryManifest`，包含：
  - 网格清单版本、双侧块版本、双侧块内容哈希；
  - 共享顶点 ID；
  - 跨块边 ID、端点及两侧块；
  - 焊接产生的稳定边界顶点 ID；
  - 上一份清单内容哈希，形成每个块对的清单版本链。
- `BoundaryManifest.MatchesChunks()` 同时检查块版本、清单版本和双侧内容哈希。
- 跨块编辑必须指定期望清单版本。若一侧块或清单变化，返回 `E_BOUNDARY_STALE`，并携带 `ChunkPair` 与期望 `ManifestVersion`。

## 并发发布与回退

- 发布版本 `MeshVersion` 不可变。查询只能获得完整版本快照，不能看到会话中的暂存块。
- `MeshSnapshotHandle` 在读毕前保持有效；`Release()` 后再次读取返回 `E_STALE_HANDLE`。
- 同一块同一时间只允许一个编辑租约；重复获取或重复提交返回 `E_BUSY`。
- 不同块可同时持有编辑会话。提交时以当前头版本为基础合并未修改块；若本会话基础块已变化，拒绝提交。
- 编辑会话在块边界检查点检查 `CancellationToken`；取消后不发布，头版本保持最后一次成功发布。
- 每个发布版本记录编辑者、操作类型、受影响块、清单版本/哈希、内容哈希和提交水位。
- 回退不是删除历史，而是追加新版本：
  - 目标版本内容被重新分块、重新计算清单与哈希；
  - 旧清单链不直接复用；
  - 旧编辑会话提交时检测到块内容已变化，返回 `E_BOUNDARY_STALE`。

完整场景中的版本序列：

```text
v1 Load
v2 Weld（跨块）
v3 FillHole（跨块）
v4 DeleteFragments
v5 RecomputeNormals（块 0）
v6 RecomputeNormals（块 1，与 v5 并发暂存）
v7 Rollback -> v1，重新生成 v7 边界清单
v8 Weld（基于回退版本）
v9 FillHole（基于回退版本）
```

## 跨块焊接与补洞

- 跨块焊接要求两个端点块均已获取租约、期望清单版本一致且当前清单哈希一致。
- 新顶点 ID 由全局分配器分配；边界清单中增加稳定标识 `BV:<hash>`，因此后续客户端可跨版本定位同一焊接点。
- 稳定标识由块对、基础版本、源顶点和坐标共同确定，写入新发布版本的清单。
- 跨块补洞要求：
  - 环 ID 来自当前基础版本；
  - 环确实跨越目标块对；
  - 两个端点块引用同一清单版本；
  - 补洞面不得退化、不得重复、不得与已有面片同顶点集合；
  - 补洞结果必须实际分配到两个端点块。
- 任一端校验失败返回 `E_FILL_FAILED`，携带 `LoopId`、`ChunkPair` 和所需清单版本；会话暂存数据不发布。

## 拼接一致性与统计

`CreateSpliceReport(version)` 做以下校验：

- 各块共享顶点坐标一致。
- 合并后顶点数、面片数与发布版本记录守恒。
- 不存在跨块重复面片（按无序顶点三元组）。
- 清单中的每条边界边仍存在，且恰好由两侧各一个面引用。
- 两侧边界边绕向相反；同向即判定翻叠。
- 若开放边两端都落在协商边界共享顶点集合中，判定为裂缝。
- 原网格作为真值来源，统计存活面片相对基线的法向角偏差均值、最大值和 P95。

任一失败返回 `E_SPLICE_INVALID`，并携带冲突 `ChunkPair`、边 ID 和清单版本。

本次验收的拼接统计：

```json
{
  "currentVertexCount": 28,
  "currentFaceCount": 33,
  "weldVertexReduction": 1,
  "filledFaceCount": 2,
  "normalDeviation": {
    "comparedFaces": 30,
    "meanDegrees": 0,
    "maxDegrees": 0,
    "p95Degrees": 0
  }
}
```

## 快照与强制终止恢复

- 每个发布版本保存一个 `snapshot-<10位版本>.json`。
- 保存先写 `.json.tmp`，`Flush(true)` 后使用同目录原子重命名。
- 文档包含布局、基线法向、全部块顶点/面片/块哈希、边界清单版本链、稳定顶点、编辑者、提交水位、语义父版本和回退来源。
- 文档自哈希在 `selfHash` 为空时对规范 JSON 计算，再写回；载入时重新计算。
- 版本链使用上一快照的自哈希连接。
- 载入扫描全部已提交 JSON：
  - 逐块重算内容哈希；
  - 逐清单重算内容哈希和版本一致性；
  - 校验版本内容哈希、基线/布局一致性及快照链；
  - 忽略 `.tmp` 残留；
  - 最新快照损坏或链不完整时，恢复最近一条完整有效链；
  - 无有效链或版本字段缺失时返回 `E_RECOVERY_INVALID`，优先定位首个异常块/字段。
- 恢复结果返回 `RecomputedBlocks` 与 `VerifiedBlocks`；本次有效快照没有未提交块，重算列表为空。
- 强杀进程本身不会写退出报告；报告仅由下一次成功启动后的恢复流程生成。

## 确定性

`verify` 不记录时间戳、进程号、绝对路径或线程调度结果。所有块、面、顶点、清单、版本和 JSON 属性都按稳定顺序输出。

相同 seed 和操作序列下的校验项：

- `traceByteIdentical=true`
- `versionsByteIdentical=true`
- `snapshotsByteIdentical=true`

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_MESH_INVALID` | 头部、字段、长度、索引、坐标、版本非法 |
| `E_BOUNDARY_STALE` | 块/清单版本不一致，跨块操作或旧会话提交过期 |
| `E_FILL_FAILED` | 环跨块、面片非法/重复、端点块未提交同一清单 |
| `E_SPLICE_INVALID` | 裂缝、翻叠、缺失边界边、重复面或数量不守恒 |
| `E_RECOVERY_INVALID` | 快照链断裂、版本缺失、块/清单哈希不符 |
| `E_STALE_HANDLE` | 已释放快照句柄再次读取 |
| `E_BUSY` | 块已有编辑任务或会话重复提交 |
| `E_CANCELLED` | 编辑在检查点观察到协作取消 |
| `E_NOT_FOUND` | 版本、块或目标不存在 |
| `E_INVALID_ARGUMENT` | 非法参数 |
| `E_IO` | 本地文件读写失败 |

## 测试覆盖

19 个 xUnit 测试覆盖：

- 二进制往返、头部字段、内容/文件哈希。
- 越界索引、截断面片载荷、缺失版本的首字段定位。
- 分块唯一归属、跨块候选依据、清单版本/哈希。
- 快照句柄隔离、释放后 `E_STALE_HANDLE`。
- 块租约 `E_BUSY`、取消保留最后发布版本、检查点协作取消。
- 不同块并发暂存、并行提交并按当前头版本合并。
- 跨块焊接的清单协商和稳定顶点 ID。
- 补洞失败原子性、环 ID/清单版本错误信息。
- 回退追加新版本、清单重算、旧会话失效。
- 边界边缺失和翻叠的拼接检测。
- 最新快照损坏回退、`.tmp` 忽略、版本字段缺失恢复错误。
- 完整工作流两次运行逐字节一致。

## 已知边界与说明

- 样本默认使用 `Float32 + UInt16`；实现同时支持 `Float64 + UInt32`。
- 法向不写入 MRM1 文件，而从发布几何确定性计算；`NormalRevision` 参与块内容哈希，所以“仅重算法向”仍会形成新的完整发布版本。
- 空间分块支持三维网格配置，但本次跨块样本集中在 X 方向两块。
- OpenGL 窗口路径依赖本地图形驱动；无图形设备时应使用 `--headless`，其软件光栅化不创建 GL 上下文。
- 原子重命名保证“文件名指向的已提交快照完整”，不承诺操作系统缓存在掉电场景下的额外持久化级别。
