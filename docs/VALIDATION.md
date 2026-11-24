# 拾取与空间操作验证说明

## 1. 交付组成

本仓库在 Go module `editorpick` 中交付仅依赖标准库的领域组件：

- `fixed.go`、`math3d.go`：声明精度为 6 位小数（`FixedScale = 1_000_000`，一步 `δ = 1e-6`）的定点数、4×4 矩阵、逆矩阵、AABB 和包围球。
- `scene.go`、`bundle.go`：场景、查询、真值、清单的 JSON 结构与 `E_SCHEMA`、`E_CYCLE`、`E_REF` 校验。
- `engine.go`、`depgraph.go`：长链世界变换、累积矩阵误差、层级包围体、依赖图和失效清单。
- `raymath.go`、`pick.go`、`pick_candidate.go`：基于 `big.Rat` 的精确射线/AABB/球/三角形求交、误差包络和 `E_AMBIG` 上下界。
- `select.go`、`point.go`：矩形框选、点包含及边界模糊判定。
- `transaction.go`：工作副本、枢轴平移/旋转/缩放、批次提交和 `E_TXN` 回滚。
- `cache.go`、`parallel.go`：查询依赖缓存、并行查询和按层级波次并行派生。
- `snapshot.go`：原子快照、内容哈希/版本/精度校验，以及损坏后整体全量重建的 `RecoverEngine`。
- `internal/samplegen`：固定种子 `241125` 的独立样本与浮点真值生成器；真值不读取组件输出。
- `internal/verify`：增量、断点恢复、全量重建三路径自动比对和异常/并发/快照检查。
- `cmd/editorpick`：`generate`、`verify`、`demo` 可运行入口。

## 2. 一键命令

跨平台 Go 命令：

```bash
go run ./cmd/editorpick generate --dir samples/default --seed 241125
go run ./cmd/editorpick verify --dir samples/default --report samples/default/verification-report.json
go test ./... -count=1
```

Windows PowerShell：

```powershell
.\scripts\verify.ps1
```

POSIX shell：

```bash
./scripts/verify.sh
```

## 3. 确定性样本

固定种子生成：

- `scene.json`：41 个节点、16 个几何体（14 个 AABB、1 个球、1 个三角网格），包含 24 级父子链和 12 个随机但种子确定的填充几何体。
- `queries.json`：9 条射线、5 个框选矩形、4 个点包含查询和 5 个枢轴编辑步骤；编辑序列含一个零缩放 `E_TXN` 步骤和一个断点。
- `truth.json`：生成器用独立 float64 矩阵/求交代码写出的每条射线-几何体相交记录、首选交点、框选集合、点包含集合和每步操作后的世界矩阵/AABB。
- `manifest.json`：上述文件的 SHA-256 清单。
- `resume-snapshot.json`、`invalidation-report.json`、`verification-report.json`：最近一次实际校验产物。

同种子连续生成两次必须逐字节相同；校验器还会比较临时生成结果与 `samples/default` 中文件的哈希。

## 4. 定点误差与模糊判定

所有持久化世界矩阵、AABB、包围球和查询结果坐标均使用 `Fixed`。矩阵乘法用 `big.Int` 汇总精确乘积后只做一次定点舍入，并按矩阵元素传播保守误差矩阵。

拾取误差上界由 `DeclaredPickErrorBound` 给出：

```text
U = Echain/δ + 2 + (depth + 1) + 2 + 2 * ceil((L + D) / 1_000_000)
ε = U * δ
```

其中：

- `Echain` 是长父子链传播后的矩阵误差（以 ULP 计）；
- `depth` 是层级深度；
- `δ = 1e-6` 是声明量化步长；
- `L` 是几何体最大局部尺寸；
- `D` 是局部射线原点距离。

射线/AABB 使用原盒、膨胀盒和侵蚀盒三态判定；球使用 `r-ε`、`r`、`r+ε`；三角形使用精确有理数 Möller–Trumbore 求交、重心边距、三角形高度下界和射线-平面夹角放大系数。无法证明确定命中或确定未命中时返回：

```json
{
  "status": "E_AMBIG",
  "ambiguity": {
    "code": "E_AMBIG",
    "lower": { "x": -3.000101, "y": -0.000102, "z": -1.000142 },
    "upper": { "x": -2.999897, "y": 0.000102, "z": -0.999938 }
  }
}
```

模糊项的节点和状态在增量、恢复、全量三条路径间逐字段比较，出现位置不一致会报告首个查询编号与字段名。

## 5. 增量与恢复一致性

校验器执行：

1. 增量路径：从 `scene.json` 构建，逐步提交枢轴批次，依赖图驱动后代派生数据和祖先聚合包围体失效。
2. 断点路径：执行到 `step-rotate-principals` 后原子写快照；重新从磁盘加载原始场景，校验快照哈希/版本，恢复节点局部矩阵、派生数据、索引和依赖图，再执行剩余步骤。
3. 全量路径：执行同一编辑序列后调用 `RebuildAll()` 全量重建。

比对内容包括：

- 射线状态、节点、交点坐标、模糊节点；
- 框选状态与完整节点集合；
- 点包含状态与集合；
- 每个节点的 16 个世界矩阵元素和 6 个世界 AABB 元素。

声明容差：

```text
matrix = 5e-4
bounds = 1.5e-3
point  = 8e-4
```

任一不一致会得到形如 `query=<id> field=<name>` 或 `step=<id> query=<node> field=world_matrix[i]` 的首个定位信息。

## 6. 快照原子性

`Engine.SaveSnapshot` 使用互斥锁串行化写入，在同目录写临时文件、`fsync`、关闭后 `rename`，避免交错或半文件。快照包含：

- schema 与结构版本；
- 定点精度参数；
- 原始场景内容哈希；
- 已提交节点局部矩阵和几何体引用；
- 全部节点派生数据、世界网格和层级索引；
- 依赖图边；
- 除 `content_hash` 外载荷的 SHA-256。

`LoadSnapshot` 发现内容哈希、结构版本或精度不匹配即返回 `E_HASH`/`E_VERSION`。`RecoverEngine` 会先做一次干净全量构建；快照无效时丢弃快照并保留该全量引擎，禁止旧派生数据与新索引混用。

## 7. 实际验证结果

执行时间：2026-11-25 00:50 CST。

执行环境：`go version go1.22.12 linux/amd64`。任务目标系统为 Windows 10/11；代码和脚本仅使用跨平台标准库、`filepath` 与原子重命名语义，并提供 `scripts/verify.ps1`。

实际命令结果：

- `go test ./... -count=1`：PASS。
- `go test ./... -count=20`：PASS。
- `go vet ./...`：PASS。
- `go run ./cmd/editorpick verify ...`：`verification passed`，报告 `ok=true`。

详细测试：

```text
--- PASS: TestSampleVerificationReport
--- PASS: TestSchemaCycleAndReferenceCodes
--- PASS: TestRayDeterminismAndAmbiguousBounds
--- PASS: TestCameraScreenRayPicksObject
--- PASS: TestTransactionFailureRollsBack
--- PASS: TestSnapshotRejectsTampering
--- PASS: TestGeometryAddRemoveInvalidation
--- PASS: TestRecoverEngineRebuildsAfterHashMismatch
```

最近一次报告中的关键数值：

```text
确定性生成/清单哈希：true / true
射线/框选/点查询/操作真值：9 / 5 / 4 / 5
最大交点偏差：3.333333339211597e-07
最大世界矩阵偏差：3.255095282983511e-06
最大世界 AABB 偏差：3.447221498031894e-06
并行查询一致：true
并行派生一致：true
缓存复用观察：true
非后代 Derived.Revision 保持不变：true
```

异常码检查全部通过：

```text
E_SCHEMA、E_CYCLE、E_REF、E_TXN、E_HASH、E_VERSION
```

说明：本 Linux 容器内直接运行 `go test -race ./...` 会因预置 `gcc` 包装器指向不存在的 `gcc-15` 而无法启用 cgo/race；这是本机工具链问题，不是测试失败。已用 `-count=20` 的高重复运行覆盖确定性和并发逻辑。Windows 安装正常 Go 工具链后可按需执行 `go test -race ./...`。
