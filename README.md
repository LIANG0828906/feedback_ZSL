# CADLab 曲面可视化与诊断工作台

从零构建的本地 CAD 曲面工作台：固定种子生成 NURBS 曲面片、细分网格与解析真值；CPU 软件渲染器提供低 LOD 预览和高 LOD 精确帧；屏幕分块缓存、依赖失效、增量重算、SQLite 检查点、预算调度、剖面测量和异常注入均可复现。

项目不依赖 Docker、系统数据库服务、账号或线上资源。运行时唯一本地状态文件是项目目录下的 `workbench.sqlite`。

## 快速验收

环境要求：Go 1.22+。本仓库固定使用 `modernc.org/sqlite v1.34.5` 和 `github.com/wailsapp/wails/v2 v2.10.2`，二者均兼容 Go 1.22，避免自动升级到要求 Go 1.26 的新版本。

```bash
go mod tidy
go test ./...
go run ./cmd/verify -out artifacts/verification.json
```

本机实际结果：`verification PASSED in 416ms`。

启动本地诊断页：

```bash
go run ./cmd/workbench -listen 127.0.0.1:34117
```

浏览器打开 `http://127.0.0.1:34117`。SQLite 默认写入当前目录的 `workbench.sqlite`，也可用 `-db` 指定。

Windows Wails 桌面构建：

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails build -platform windows/amd64 -tags wails -s
```

`-s` 表示跳过前端构建：诊断界面是原生 HTML/CSS/JS，不需要 Node/npm。本次已在当前环境交叉构建出约 14MB 的 Windows 可执行文件；Linux 桌面构建需要额外安装 GTK/WebKit 开发库，未安装时可使用上面的 HTTP 诊断页。

## 已实现能力

| 任务要求 | 实现位置 | 验证 |
|---|---|---|
| 确定性 NURBS 样本、控制网格、节点、权重、裁剪边界、细分网格、真值 | `internal/sample` | `internal/model/validation_test.go` |
| 结构校验与 `E_SCHEMA` / `E_WEIGHT` / `E_CLIP` / `E_KNOT` / `E_GEOM` | `internal/model/validation.go` | 错误码、曲面片编号、字段名断言 |
| 解析偏导、逐顶点法线、曲率联动细分、裁剪三角形 | `internal/geom` | 真值坐标/法线容差、面数单调性、`E_PARA` 测试 |
| 预览与精确双路径、同一相机/裁剪/版本、LOD 与路径记录 | `internal/render`, `internal/engine` | 同版本元数据校验、预览 50ms 测试 |
| 曲面片/控制点/材质/密度/分块/着色阶段依赖图 | `internal/deps` | 显式节点和边、分块覆盖失效、`E_SCOPE` 审计 |
| 精确分块增量重算、未失效块复用、全量逐像素比对 | `internal/engine/render_paths.go` | 像素、覆盖率、明暗直方图全等 |
| 首个不一致像素、分块、字段定位 | `internal/render/full.go` | `E_DIFF` 定位测试 |
| SQLite 逐块事务、稳定顺序、场景+缓冲检查点 | `internal/store` | 写入顺序 0..39、重启恢复测试 |
| 内容哈希/版本冲突整体作废、`E_SNAP` 冲突块 | `internal/store/sqlite.go` | 篡改单块后返回冲突分块并全量重算 |
| 强制中断/不完整快照 `E_RECOVER` | `internal/engine`, `internal/store` | 只提交部分分块后重启测试 |
| 最大并行数、像素/网格缓冲预算、排队原因与等待时长 | `internal/sched` | 峰值、预算、取消、无丢块测试 |
| 用户取消界面入口、取消后仅信任已提交检查点 | `internal/app`, `frontend/dist` | 取消映射为 `E_RECOVER`，不产生半成品完成帧 |
| 剖面平面交线、世界坐标+UV 双读数、版本冻结 | `internal/engine/profile.go` | 106 个交线段、A/B 双端点和锁定版本 |
| 四类异常注入与逐块重试 | `internal/engine/anomaly.go` | `E_GEOM` / `E_KNOT` / `E_PARA` / `E_STAGE` |
| 分块矩阵、失效传播、缓存命中、三路径一致性和导出 | `frontend/dist`, `internal/app` | HTTP API 与桌面 UI 共用 |

## 架构

```text
cmd/workbench      HTTP 诊断服务，无 GUI 依赖
cmd/verify         可复现验收命令，输出 artifacts/verification.json
main_wails.go      //go:build wails 的桌面入口
frontend/dist      无框架诊断界面
internal/app       统一 Call(action, payload) API
internal/engine    快照、双路径、增量、恢复、剖面、异常
internal/render    CPU 光栅化、分块、相机、统计和逐像素比较
internal/deps      控制点/材质/密度/曲面片/分块/着色依赖图
internal/sched     确定性结果收集、受限并发和缓冲预算
internal/store     modernc.org/sqlite 检查点
internal/geom      NURBS 求值、解析偏导、曲率和裁剪细分
internal/sample    固定种子样本和独立真值求值
internal/model     数据模型、校验和稳定诊断错误
```

## 渲染与失效语义

- 屏幕固定切分为 8×5（可配置）分块。每次精确渲染只重算依赖图标记失效的块，其余块直接复用内存缓存。
- 控制点编辑按 B 样条控制点支撑域筛选三角形；材质只影响覆盖曲面片的分块的着色阶段；密度编辑只使对应曲面片覆盖块及其下游着色阶段失效。
- 增量结果完全组装后，与从空缓存渲染的全量参考帧逐像素、逐块轮廓像素数、分块覆盖率、16 档亮度直方图比较。
- 若未失效块出现差异，返回 `E_SCOPE` 并列出误标分块；若已重算块仍不一致，返回 `E_DIFF`，附带首个差异像素、分块和字段。
- 预览和精确帧共享同一 `SceneState` 版本、相机内外参数和裁剪平面。混用不同快照时诊断中带出两侧版本号。

## 持久化与恢复语义

SQLite 表：

- `snapshots`：版本、内容哈希、算法版本、视口/分块参数、场景 JSON、完整标记。
- `tile_checkpoints`：分块颜色缓冲、深度缓冲、统计、内容哈希、算法版本和稳定序号。

每次精确/增量提交都先写新快照元数据，再按分块序号执行独立事务，最后标记完整。写入由互斥锁串行化；颜色和深度缓冲使用 gzip 存储。

重启时通过保存的场景 JSON 恢复最后编辑版本。任何快照级或逐块级内容哈希/算法版本冲突都会删除整个快照并触发全量重算，返回 `E_SNAP`；只有已提交但未标记完整时返回 `E_RECOVER`，不会承诺强制终止后仍能生成完整帧。

## 异常与诊断

稳定错误码：

- `E_SCHEMA`, `E_WEIGHT`, `E_CLIP`, `E_GEOM`, `E_KNOT`, `E_PARA`
- `E_VER`, `E_SCOPE`, `E_DIFF`, `E_SNAP`, `E_RECOVER`, `E_STAGE`

诊断对象统一包含 `code`、`message`、`patchId`、`tileId`、`field`，按适用情况附 `versionA/versionB`、`conflictingTiles`、`mislabeledTiles`、`reasons`、`firstPixel`。

界面左侧的故障注入按钮不会破坏当前有效快照；单分块失败可在右侧矩阵选中该块后重试，重试结果写回缓存并更新检查点。

## 本次实际验收结果

完整机器可读结果见 `artifacts/verification.json`。关键数值：

```text
seed                         20261124
patches / meshes             3 / 2
truth samples                243
max coordinate error         0
max normal angle error       0.00000148 deg
preview                      LOD 10, 40 tiles, 17.221 ms render / 21.340 ms incl. PNG
exact                        LOD 48, 40 tiles, 33.725 ms
material edit P2             invalid 2 / reused 38, pixel/coverage/shading equal
control edit P1              invalid 3 / reused 37, pixel/coverage/shading equal
density edit P3              invalid 3 / reused 37, pixel/coverage/shading equal
resume                       version 4, 40 committed tiles, pixel equal
checkpoint order             [0..39] stable
queue                        peak parallel 2, pixel 0.121 MB, mesh 0.944 MB
profile                      106 segments, locked to version 4
anomalies                    E_GEOM, E_KNOT, E_PARA, E_STAGE
```

验收命令同时比较重启恢复帧与全量精确帧，结果为逐像素相等；所有编辑路径的分块轮廓覆盖率和明暗直方图均相等。

验收命令同时生成 `artifacts/preview-frame.png` 和 `artifacts/exact-frame.png`，可直接查看两条渲染路径的实际画面。

计时会随机器负载小幅波动，以最新 `artifacts/verification.json` 为准。

## 界面使用

1. 点击“预览帧”检查低 LOD 响应，点击“精确帧”建立完整分块缓存。
2. 修改控制点/密度/材质后应用；右侧矩阵黄色块为本次失效块，绿色块表示缓存复用。
3. 点击“三路径比对”查看全量、增量、恢复帧的一致性及首个差异位置。
4. 调整剖面 Y 偏移并提取交线；下方显示 A/B 端点的世界坐标与参数域 UV 读数。
5. 注入异常查看错误码、曲面片和分块定位；选中失败块可逐块重试。
6. 渲染遮罩上的“取消当前渲染”会中止当前任务；取消后只能恢复已提交且版本匹配的检查点。
7. “导出”下载包含场景、依赖图、失效清单、队列、比较结果和帧元数据的 JSON。

## 测试

```bash
go test ./... -count=1
go test -race ./internal/... -count=1
go test ./internal/engine -run TestPreviewExactIncrementalAndResume -v
go run ./cmd/verify -out artifacts/verification.json
```

测试覆盖细分真值精度、法线夹角、密度单调性、四类校验错误、预览时限、双路径元数据、增量/全量一致性、错误范围审计、版本错误、差异定位、检查点顺序、快照冲突、不完整恢复、预算并行、取消、剖面和异常链路。
