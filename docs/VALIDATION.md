# CAD 曲面管线交付与验证说明

本交付是一套无外部服务、确定性的 CAD 参数曲面处理、软件渲染、增量失效和 SQLite 检查点工具。样本由固定种子 `420261125` 在本地生成，不依赖 Docker、网络素材或未授权服务。

## 一键复现

```bash
./scripts/acceptance.sh
```

等价的分步命令：

```bash
go test ./...
go run ./cmd/samplegen -out samples -seed 420261125
go run ./cmd/cadpipe verify --workers 4 \
  --report artifacts/verification-report.json \
  --work artifacts/acceptance
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

强制终止恢复演练：

```bash
db="artifacts/crash-demo.sqlite"
./artifacts/cadpipe crash --db "$db" --after 5
./artifacts/cadpipe recover --db "$db"
```

Unix 下 `crash` 在第 5 个分块事务提交后向自身发送 `SIGKILL`，Shell 记录退出码 `137`；强杀后不会生成退出报告。Windows 端口使用 `taskkill /F /PID` 实现等价硬终止。

## 实际验证结果

本次工作区中的实际结果：

- `go version`: `go1.22.12 linux/amd64`
- Wails CLI: `v2.10.2`
- `go test ./...`: `cadpipe/internal/pipeline`、`schema`、`tessellate` 全部通过。
- `go vet ./...`: 退出码 `0`。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`: 退出码 `0`。
- `cadpipe verify`: `12/12` 检查通过，报告见 `artifacts/verification-report.json`。
- 一键脚本实际强杀结果：`crash_exit_code=137`，恢复输出 `recovered=5 resumed=91 checkpoint_rows=96 pixel_match=true`。
- 再次恢复已完成快照：`recovered=96 resumed=0 checkpoint_rows=96 pixel_match=true`。

12 项验收覆盖：

1. 三版本双向流转、重复导出逐字节一致和 `E_LOSSY`。
2. 解析采样真值、包围盒和细分一致性。
3. 裁剪探针逐点一致、自交/越界 `E_CLIP`。
4. 单线程与 4 线程渲染的逐分块哈希和像素稳定。
5. 控制点精确失效及增量/全量逐像素一致。
6. 材质精确失效及增量/全量逐像素一致。
7. 细分参数精确失效及增量/全量逐像素一致。
8. 裁剪翻转集合与失效分块清单互相印证。
9. 取消后仅恢复已提交分块，续算帧与全量一致。
10. 场景版本不匹配时整快照作废并清空旧行。
11. 像素缓冲池耗尽阻塞、等待原因和耗时记录。
12. `E_DIFF` 定位首个分块、像素坐标和差异字段。

## 代码地图

- 三版本加载/导出：`internal/schema/load.go:15`、`internal/schema/export.go:9`。
- 错误码与统一领域模型：`internal/cad/errors.go`、`internal/cad/types.go`。
- 裁剪判定与 `E_CLIP`：`internal/cad/clip.go`。
- NURBS 求值、法线、曲率和细分：`internal/tessellate/nurbs.go`、`internal/tessellate/tessellate.go:100`。
- 相机内外参、投影和分块：`internal/render/camera.go`。
- 确定性软件光栅、覆盖率、明暗统计和哈希：`internal/render/render.go:138`。
- 覆盖分块索引：`internal/render/coverage.go`。
- 六类语义版本与编辑入口：`internal/pipeline/versions.go`、`internal/pipeline/edit.go:56`。
- 拓扑清单：`internal/pipeline/dag.go`、`internal/pipeline/plan.go`。
- 并行池、有序串行提交、取消边界：`internal/pipeline/pool.go`、`internal/pipeline/run.go:27`。
- 增量/全量比对器：`internal/pipeline/compare.go`。
- SQLite 原子检查点和整快照失效：`internal/store/store.go:114`、`internal/store/store.go:192`。
- 固定种子样本生成器：`internal/sample/generator.go`。
- 端到端验收集合：`internal/acceptance/run.go:26`。
- CLI：`cmd/cadpipe/main.go`、`cmd/samplegen/main.go`。

## Schema 差异实现

三个受支持版本为：

- `cad.scene/v1`: snake_case 字段；绝对坐标；毫米；`clip_boundary.uv_points`；有材质字段，无细分字段。
- `cad.scene/v2`: camelCase 字段；`controlBaseMm + controlDeltasMm` 增量坐标；毫米；`clip.flatUV`；无材质和细分字段。
- `cad.scene/v3`: PascalCase 几何字段；米；`ControlGridM`；`KnotVectors` 与 `Clipping.Rings`；包含材质和细分可选字段。

内部长度使用整数纳米 `cad.Length` 作为规范表示，因此毫米/米往返不会累积浮点换算误差。未知版本、未知单位或必填字段缺失返回 `E_VER`，错误包含 `version` 和 JSON 字段路径。低版本升级时，默认材质/细分的补齐会写入 `Transcript.Entries`，包含字段路径、原缺失值 `null`、处置动作和填充值。

导出到低版本时，不支持的字段进入 `LossReport.Entries`，每项代码为 `E_LOSSY`，并记录 JSON 路径、原值和 `omit_unsupported_field_*` 处置方式。`json.MarshalIndent` 加固定结构字段顺序保证同一内部模型重复导出逐字节一致。

## 几何与渲染确定性

样本曲面是二次 Bézier/NURBS 网格，解析形式为：

```text
x = x0 + width*u
y = y0 + height*v
z = 16*amplitude*u*(1-u)*v*(1-v)
```

3×3 控制点、夹紧节点和单位权重精确表示该曲面；生成器独立写出解析采样点、精确包围盒和裁剪探针。核心 NURBS 使用标准跨距/Cox–de Boor 基函数，偏导通过确定性有限差分得到法线、二阶导和曲率近似。

细分段数由基础密度、曲率项和容差弦差项共同决定。测试验证：

- 密度 `1,2,4,8,16` 时三角形数单调不减。
- 容差 `3e-3 ... 3e-5` 收紧时面数不减、最大近似误差不增。
- 高曲率参数比平面产生更多分段。
- 密度越界返回 `E_PARA`，节点向量非单调返回 `E_KNOT`。

渲染器使用固定相机参数、固定光照公式、逐像素深度比较和稳定的曲面片 ID 排序。分块内容哈希由投影三角形/UV/裁剪掩码摘要与材质版本及参数指纹共同决定；缓存键再组合内容哈希、场景结构版本、相机版本和算法版本。线程 ID、任务完成顺序和缓存命中不进入任何哈希或像素公式。

## 精确失效与裁剪语义

每个曲面片维护六类版本分量：

```text
control, knot, weight, clip, material, subdivision
```

控制点、节点、权重或细分编辑会合并旧/新投影覆盖分块，避免曲面移动后旧分块漏失效。材质编辑失效当前覆盖分块。清单中的每项包含 `tile_id`、分块坐标、`patches`、`reasons`，裁剪编辑还包含 `flipped_probes`。

裁剪编辑不做全量失效：先比较真值探针在旧/新边界下的内外结果，只把翻转探针投影到对应分块并加入清单。验收样本中的局部切角只翻转 `surface-alpha-upper-right-inside`，且只失效分块 `39`；翻转集合和清单完全一致。

未失效分块直接复用内存和 SQLite 中的颜色/深度缓冲，测试逐字节比较 `ContentHash` 与 `Color`。失效分块渲染完成后先复制出不可变结果并释放像素缓冲，再进入按分块编号排序的提交队列，避免乱序结果占满缓冲造成队头死锁。

## SQLite 检查点与恢复边界

`checkpoints` 表在一个事务中写入：

- `color_blob` 与 `color_digest`
- `depth_blob` 与 `depth_digest`
- `content_hash` 与重算的 `cache_key`
- `scene_structure_version`
- `camera_version`
- `algorithm_version`
- 该分块依赖的语义版本哈希
- 覆盖率、轮廓覆盖率和明暗统计 JSON

`Store.Save` 使用互斥锁和单条 UPSERT 事务；管线按分块编号顺序提交，因此写入不会交错，`tile_order` 稳定。SQLite 驱动为纯 Go 的 `modernc.org/sqlite`，PRAGMA 使用 `synchronous=FULL`。

重启时 `LoadCompatible` 会：

1. 重算颜色和深度摘要，拒绝损坏 BLOB。
2. 用持久化内容哈希和当前版本重算缓存键。
3. 比较当前曲面片语义依赖哈希。
4. 比较场景、相机和算法版本。
5. 任一行不匹配就在一个事务中清空整个检查点表并返回 `SnapshotMismatchError`，禁止新旧分块混用。

取消或真实 `SIGKILL` 只会留下已经完成事务的前缀分块。重新打开后，版本匹配的分块被恢复，缺失分块续算；最终帧与空缓存全量帧通过同一个 `CompareTileMaps` 逐像素、逐深度、逐摘要和逐统计字段比较。

## 并发与资源控制

- Worker 数由 `Options.Workers` 限制，默认 `min(4, GOMAXPROCS)`。
- `BufferPool` 限制同时借出的像素缓冲数量和单缓冲像素上限。
- 当所有缓冲被借出时，任务阻塞在通道上；获得缓冲后记录 `pixel_buffer_exhausted`、分块号和等待时长。
- 渲染结果在进入有序提交队列前复制并释放缓冲，防止有序提交队头被乱序任务锁死。
- 通道和 `WaitGroup` 等待所有任务结束，错误不会静默丢块。

## Wails 状态与 Windows 说明

当前环境已安装 Wails CLI `v2.10.2`，`wails doctor -nocolour` 的完整输出保存在 `artifacts/wails-doctor.txt`。CLI 本身可运行；Linux 侧诊断缺少 `libwebkit2gtk-4.0-dev`，因此本环境没有把 Wails GUI 打包作为验收路径。

通过 WSL 互操作探测到的 Windows 侧 `powershell.exe` 中没有 `go`/`wails` 的 PATH 项，所以没有把“Windows 原生 `wails doctor` 已运行”记为事实验证结果。Windows 原生环境应执行：

```powershell
go version
go install github.com/wailsapp/wails/v2/cmd/wails@latest
$env:Path = "$(go env GOPATH)\bin;$env:Path"
wails doctor
```

核心模块和 `cadpipe` CLI 已通过 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`。后续 Wails 桌面壳可直接绑定 `internal/pipeline` 与 `internal/acceptance`，不需要重写管线。

## 已知验证限制

- 当前环境的 `-race` 需要 CGO 和可用 GCC；`/home/liang/.local/bin/gcc` 包装器指向不存在的工具链路径，因此未取得成功的 race 报告。普通 `go test`、`go vet` 和端到端并发验收均通过。
- 渲染器是面向确定性验证的软件光栅器，不是生产 GPU 渲染器；其价值是让增量/全量差异可精确定位到像素和字段。
