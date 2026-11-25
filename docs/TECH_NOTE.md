# 地形高程 → 切线法线烘焙管线技术说明

本原型在无外部素材、无网络服务依赖的前提下，实现确定性样本生成、结构校验、法线/坡度烘焙、接缝归因、依赖失效、预算调度、SQLite 版本检查点、三路径一致性比对，以及 Wails 诊断工作台。

## 1. 快速命令

在仓库根目录执行：

```bash
go run ./cmd/terrainbake generate --out artifacts/sample
go run ./cmd/terrainbake bake \
  --project artifacts/sample \
  --out artifacts/demo \
  --workers 3 --pixel-buffer 16 --memory-bytes 4096
go run ./cmd/terrainbake verify --work artifacts/verification
go run ./cmd/terrainbake serve \
  --report artifacts/verification/verification-report.json \
  --assets artifacts/verification/art-full
```

一键顺序执行生成、烘焙和验收：

```bash
go run ./cmd/terrainbake all --out artifacts
```

完整自动化测试：

```bash
go test -count=1 ./...
```

桌面诊断界面：

```bash
cd desktop
wails dev
# 或 Windows 10+：wails build
```

SQLite 使用 `modernc.org/sqlite` 纯 Go 驱动，不需要 gcc/CGO。桌面模块通过根模块公共门面 `studio_sdk.go` 调用核心能力。

## 2. 确定性样本

固定种子：`model.FixedSeed = 0x51EED2026`，十进制 `21993693222`。

样本包含 7 块不同分辨率地形：

- 上行：`B01 8×6`、`B02 10×6`、`B03 6×6`
- 下行：`B04 8×8`、`B05 10×8`、`B06 6×8`
- 隔离块：`B07 7×5`

接缝：

- `S01 B01↔B02`：普通曲率/差分样本
- `S02 B02↔B03`：断层式 `GEOMETRIC_CREASE`
- `S03 B04↔B05`：`BAKE_MATERIAL_MIX`
- `S04 B05↔B06`：二阶曲率导致的 `BAKE_DISCRETIZATION`

材质层为 `base`、`rock`、`moss`，权重为 `0..255` 量化整数且每像素和为 `255`。`rock` 仅由 `B02/B04` 使用，`moss` 仅由 `B03/B05` 使用，用于验证材质参数影响范围。

## 3. 结构校验与异常码

加载入口：`internal/projectio/projectio.go`。

| 码 | 触发条件 | 上下文 |
|---|---|---|
| `E_SCHEMA` | 顶层字段缺失、JSON 类型错误、权重不是整型、权重越界、差分算子非法 | 块标识 + 字段路径 |
| `E_SIZE` | 高程数组长度/宽高不符、权重像素数不符、共享边采样长度不等、真值像素越界 | 块标识 + `elevation`/`weights`/`seams...` |
| `E_REF` | 接缝引用不存在的 `block_a/block_b`、真值引用不存在 | 被引用块 ID + 字段名 |
| `E_SEAM` | 归因信号落在不可判定区间 | 块对、两个像素坐标、实测夹角、两侧法向量 |
| `E_DIFF` | 三路径产物不一致 | 首个不一致块、产物类型、两侧摘要 |
| `E_BUDGET` | 单块法线图超过内存预算 | 块 ID、所需字节、预算字节 |

加载器遇到异常立即返回带上下文的 `*model.Error`，不会静默跳过异常块。

## 4. 法线烘焙

核心：`internal/bake/bake.go`。

- 支持 `central`、`forward`、`sobel` 差分算子。
- 切线空间法线：`normalize(-s * dH/dx, -s * dH/dz, 1)`，其中 `s = HeightScale`。
- 材质层先产生层法线，再按量化权重线性混合并归一化。
- 输出归一化分量、`0..255` RGB 颜色码和坡度角。
- `BakeBlockTiled` 使用有上限的每 worker 像素缓冲分块计算；测试验证其与整图烘焙逐像素相同。
- 烘焙核不读取 goroutine ID、调度顺序或墙钟时间；耗时只存在于诊断统计中。

## 5. 接缝归因与连续性

核心：`internal/seams/seams.go`。

每个接缝像素记录：

- `raw_angle_deg`：材质混合后的原始烘焙夹角。
- `geometry_angle_deg`：绕过材质、使用当前差分算子得到的几何法线夹角。
- `basis.corrected_geometry_angle_deg`：用二阶曲率修正边界一阶差分偏差后的夹角。
- `alternative_angle_deg`：跨接缝高程重建的 seam-aware 法向夹角。
- `basis.edge_height_jump`、`basis.cross_slope_deg`。

判定规则：

- 曲率修正后仍 ≥ 阈值：`GEOMETRIC_CREASE`。
- 原始角显著大于无材质几何角且几何角未严重超标：`BAKE_MATERIAL_MIX`。
- 曲率修正或跨缝重建后降到阈值内：`BAKE_DISCRETIZATION`。
- 信号冲突且均不满足上述规则：`AMBIGUOUS`，并返回 `E_SEAM`。

可安全修复的烘焙伪突变在组合阶段用法向均值焊接；真实折角只做受限焊接，避免把断层伪造成连续曲面。不可判定点不强行焊接。未编辑样本的所有接缝最终夹角均在 `6°` 声明阈值内；验收中故意编辑边缘像素后会产生一个带完整块对/坐标/法向上下文的真实 `E_SEAM` 诊断。

## 6. 依赖图与精确失效

核心：`internal/depgraph/graph.go`。

图中显式记录：

- `source:<block> -> raw:<block> -> final:<block>`
- `raw:A -> seam:S -> final:A/B`
- `layer:<material> -> raw:<used block>`
- `param:differential -> raw:*`

块级接缝依赖同时保存 A→B 和 B→A。由于双向接缝依赖形成环，调度采用两阶段：所有 `raw` 块任务可并行，完成后经 seam join 再组合最终块；稳定块 ID 顺序是 raw DAG 的合法拓扑序。

固定样本上的实测失效集合：

| 编辑 | 失效块 | 复用块 |
|---|---|---|
| `B01` 高程 | `B01,B02` | `B03..B07` |
| `rock` 层参数 | `B01,B02,B03,B04,B05` | `B06,B07` |
| 差分算子 | 全部 7 块 | 无 |

计划条目包含 `invalidated`、`will_bake`、`reused` 和完整原因链。

## 7. 预算调度与 SQLite 检查点

调度器：`internal/budget/scheduler.go`。

- `MaxWorkers` 限制并行 worker。
- `PixelBufferPixels` 限制每 worker 的 `[]PixelBake` 长度，大块按该长度分 tile。
- `NormalMapMemoryBytes` 限制同时在制法线图字节；不足时在条件变量上等待，并记录 `normal_map_memory` / `normal_map_memory_acquired`。
- worker 槽耗尽时记录 `max_workers`。
- 单块超过总预算返回 `E_BUDGET`，不会丢块。

检查点：`internal/checkpoint/store.go`。

每张块快照在一个 SQLite 事务内写入：

- 法线摘要、坡度摘要、权重摘要、结果哈希
- 接缝统计 JSON、依赖集合 JSON
- `content_hash`、`param_version`、`algo_version`
- append-only `commit_log` 与当前 `heads`

`*sql.DB` 设为单连接，`Store` 另有互斥锁；提交循环按稳定块序逐块事务写入，因此不会交错。文件缓存采用 `*.tmp + rename` 原子替换。

重启时逐块比较当前 `BlockCacheHash`、`param_version`、`algo_version` 与 head；全局 `CheckCompatibility` 也能依据整体内容哈希或参数版本判定旧快照作废。

## 8. 三路径一致性与崩溃恢复

`internal/verify/verify.go` 的 `RunAll` 实际执行：

1. 生成并重新加载校验样本。
2. 实时增量路径：基础烘焙 → B01 高程编辑 → rock 参数编辑 → 算子切换。
3. 恢复路径：重复同一编辑序列，在最终轮提交 2 块后返回 `pipeline.ErrCrash`；关闭并重开 SQLite，以 `Resume:true` 补齐剩余块。
4. 空缓存全量路径。
5. 单 worker、小缓冲串行路径。
6. 用法线分量、RGB、坡度、摘要、接缝 JSON 和导出目录 SHA-256 交叉比较。

最终轮三条路径的提交身份序列均为：

```text
B01@9787ab46ee=e7d6d589c9 → B02@52ee8219e2=c4354d16f7
→ B03@dfc5b70f9d=1a31bcfea2 → B04@1cf0d35a62=6327202940
→ B05@44309e3241=fda8f0f5af → B06@973ce96dc7=edbd172f4b
→ B07@8baf3270a2=0a83cbe3f3
```

`@` 前是内容哈希前缀，`=` 后是最终结果哈希前缀。绝对 SQLite `seq` 会包含此前编辑轮次，因此比较的是同一 generation 内的有序提交身份。

注入崩溃后只有前 2 块 durable commit；系统不会声称崩溃瞬间拥有完整产物。重开后已提交块被识别为 recovered，剩余块补齐，最终序列与全量一致。

## 9. 单块异常隔离

验收中向隔离块 `B07` 注入确定性烘焙错误：

- 首次运行：`failed_blocks.B07` 存在，`B07` 无结果、无提交；其余 6 块正常烘焙/提交。
- 重试：清除注入错误并以 resume 方式重跑；`B07` 成功，其他块复用快照。
- 重试后的 7 块结果与无故障健康路径通过 `compare.BakeOutputs` 逐像素比较。

## 10. 实际验证结果

本次交付环境为 Ubuntu 26.04、Go `go1.22.12 linux/amd64`、Wails CLI `v2.10.2`。

根模块测试：

```text
$ go test -count=1 ./...
ok  terrainpipeline
ok  terrainpipeline/internal/bake
ok  terrainpipeline/internal/depgraph
ok  terrainpipeline/internal/seams
```

完整验收：

```text
$ go run ./cmd/terrainbake verify --work artifacts/verification
verification: 43/43 checks passed
```

报告：

- `artifacts/verification/verification-report.json`
- `artifacts/verification/verification-report.md`

接缝归因计数（未编辑基础样本）：

```json
{"BAKE_DISCRETIZATION":5,"BAKE_MATERIAL_MIX":1,"GEOMETRIC_CREASE":6,"NONE":16}
```

预算事件：

```json
{
  "parallel": {"normal_map_memory":4,"normal_map_memory_acquired":4},
  "serial": {"max_workers":5,"normal_map_memory":1,"normal_map_memory_acquired":1}
}
```

异常矩阵全部符合预期：`E_SCHEMA`、`E_SIZE`、`E_REF`、权重 `E_SCHEMA`、`E_SEAM`、`E_BUDGET`、`E_DIFF`，以及内容哈希/参数版本快照失效检查。

Web 看板实测：

```text
/                                  -> 200 text/html
/api/report                        -> 200 application/json
/api/seams                         -> 200 application/json
/assets/normal/B01.png             -> 200 image/png
/assets/slope/B01.png              -> 200 image/png
```

Wails 桌面模块的 Windows 纯 Go 交叉编译已通过：

```text
$ cd desktop
$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# exit 0
```

当前 Linux 容器没有 `pkg-config` 和 `webkit2gtk-4.1` 开发包，因此未在此环境执行原生 Linux Wails 窗口；Windows 10+ 安装 Go 1.22、Wails CLI 和 WebView2 后可直接 `cd desktop && wails dev` 或 `wails build`。CLI、SQLite、算法和 Web 诊断看板均已在当前环境实际运行。
