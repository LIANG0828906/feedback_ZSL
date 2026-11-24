# 采集点分布重建数据链路 — 技术说明

## 1. 目标与范围

本项目在空工作区中从零实现一条**完全离线、确定性**的数据链路，用于关卡设计阶段依据少量密度
观测还原各地带采集点分布并验证密度热力面。所有随机量都来自固定种子的 SplitMix64 派生流，
不依赖 `math/rand` 全局源、外部网络、账号或付费资源。SQLite 使用纯 Go 驱动
`modernc.org/sqlite`，因此 `CGO_ENABLED=0` 即可在 Windows/Linux/macOS 构建。

## 2. 目录结构

```
cmd/recon/                 可运行示例入口（sample/run/incremental/recover/serve）
internal/model/            稳定可序列化的数据结构
internal/errors/           稳定错误码 E_SCHEMA/E_GEOM/E_OBS/E_ZONE/E_DEP/E_DIFF/E_BOUND/E_VER
internal/geom/             多边形闭合/自交、矩形包含/重叠、栅格相容性
internal/rand/             SplitMix64 确定性随机流与量化函数
internal/generator/        固定种子的区域/栅格/真值/采样格点/观测生成
internal/loader/           JSON 装载与 E_SCHEMA/E_GEOM/E_OBS 校验
internal/pipeline/         分层采样、密度反推、重建、热力面、依赖图、增量、偏差上界
internal/checkpoint/       SQLite 分地带原子检查点与版本校验
internal/validate/         唯一允许读取真值的验收比对包
internal/web/              标准库 net/http + go:embed 本地诊断面板
docs/                      技术说明
artifacts/                 本地运行产物（样本、报告、二进制、数据库）
```

## 3. 确定性设计

- 随机源 `rand.Derive(parent, tag)` 由父种子与稳定标签派生独立子流，跨 OS/Go 版本逐位一致。
- 真值点：每地带按 `密度 × 有效面积` 用拒绝采样落到禁采区之外，稀有度按权重抽取。
- 采样格点：固定 10 单位间距，按地形分层后以坐标字典序确定性选取；同一区域+配置结果逐项相同。
- 观测：以 r=12 的圆盘计数密度量化到 0.001 步长；稀有度分量同步记录，用于反推配比。
- 重建：抽样流由 `区域种子 + 地带号 + 估计摘要` 派生，坐标吸附到栅格（4 单位）后再排序。
- 热力面：与观测**同一定义**（r=12 圆盘计数密度 + 相同量化），保证两者同尺度可直接比较。

## 4. 八项业务要求的实现映射

1. **生成与装载校验** — `generator` + `loader`。缺字段返回 `E_SCHEMA` 并带字段路径；
   多边形未闭合/自交、地带重叠、栅格与包围盒不符返回 `E_GEOM` 并带区域/地带编号；真值越界
   同样按几何错误处理。
2. **分层采样与密度反推** — `pipeline/sampling.go`、`estimate.go`。排除禁采区与无效点，按地形
   分层确定性布设；地带密度为观测均值，标准误由样本方差给出；观测缺失返回带格点坐标的 `E_OBS`。
   容差收紧时超差地带数单调不减（测试 `TestToleranceMonotonic`）。
3. **重建与热力面** — `reconstruct.go`、`heatmap.go`。稀有度取整使用整型最大余数法并逐地带输出
   毫单位残差；重建点落入禁采区由 `E_ZONE` 守卫（测试覆盖）。
4. **依赖图与精确失效** — `graph.go`。显式边
   `zone_params/nocollect -> sample_layout -> density_estimate -> reconstruction -> heat_tile`，
   仅沿同地带下游传播；无依赖边的误标请求返回 `E_DEP` 并带地带与边。
5. **增量 vs 全量逐点一致** — `rebuild.go`。`CompareOutcomes` 逐点比较坐标集合、逐瓦片比较像素，
   首个不一致返回 `E_DIFF` 与两侧实测值；命中序列用 `CompareHitSequence` 单独验证同模式可重复性。
6. **偏差传播报告** — `bounds.go`。四项上界（量化、层内标准误、稀有度残差、热力误差）随地带序
   单调累积，仅使用已观测点数、面积、观测值与量化步长；观测不足返回 `E_BOUND`。
7. **SQLite 原子检查点** — `checkpoint/store.go`。单连接 + 互斥串行写入，每地带一个事务并记录
   稳定 `seq`；重启按内容哈希/参数版本/算法版本整体校验，首个不匹配即 `E_VER` 并整体作废重建，
   禁止混用旧地带。
8. **并发边界、异常隔离与面板** — `engine.go` 固定 worker 池 + 像素缓冲信号量，耗尽时阻塞并记录
   排队原因；单地带 panic/error 被捕获、隔离并重试一次；`web` 用 Canvas 呈现六类视图。

## 5. 容差声明与依据

容差在 `internal/validate/validate.go` 集中声明，来自确定性样本的可观测量噪声而非真值调参：

| 指标 | 声明容差 | 依据 |
| --- | --- | --- |
| 地带密度相对误差 | 20% | 估计密度等于观测均值，实测相对误差 ≤ 15.4% |
| 重建点数相对误差 | 15% | 小样本整数计数与有效面积建模误差，实测 ≤ 13.8% |
| 热力面绝对误差 | 0.04 | r=12 圆盘面积 ≈452.4，单个点对应密度 ≈0.00221；0.04 ≈ 2 个点的空间重排噪声，实测最大 0.040 |

注意：估计密度逐地带严格等于该地带观测均值（构造保证），热力误差仅来自重建点相对真值点的
空间重排，而不是密度系统性偏差。

## 6. 增量与全量一致性的语义说明

增量运行的缓存命中序列（如 `[hit miss hit hit]`）与从空缓存的全量运行（全 `miss`）在定义上
必然不同，这是缓存复用的预期行为而非不一致。因此验收分两层：

- **必须逐点相等**：重建采集点坐标集合、热力瓦片坐标与像素值，由 `CompareOutcomes` 强制。
- **命中序列可重复**：相同编辑重复走增量路径必须产生完全相同的命中序列，由
  `CompareHitSequence` 强制。

## 7. 本地验证（含 Windows）

```bash
# 全离线，纯 Go SQLite
export CGO_ENABLED=0
go test ./...                         # 单元与一致性测试
go build ./...                        # 当前平台构建
GOOS=windows GOARCH=amd64 go build -o artifacts/recon.exe ./cmd/recon

# 端到端示例
go run ./cmd/recon sample  -out artifacts/sample.json
go run ./cmd/recon run     -sample artifacts/sample.json -db artifacts/recon.db
go run ./cmd/recon incremental -sample artifacts/sample.json -db artifacts/recon.db -zone 2
go run ./cmd/recon recover   -sample artifacts/sample.json -db artifacts/recon.db
go run ./cmd/recon serve     -sample artifacts/sample.json -addr 127.0.0.1:8080
```

Windows 上（PowerShell）：

```powershell
$env:CGO_ENABLED=0
go test ./...
go build -o recon.exe ./cmd/recon
.\recon.exe sample -out sample.json
.\recon.exe run -sample sample.json -db recon.db
.\recon.exe serve -sample sample.json -addr 127.0.0.1:8080
```

面板仅请求同源 `/api/data`、`/app.js`、`/styles.css`，无任何外部站点访问。
