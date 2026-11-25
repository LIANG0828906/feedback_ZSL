# LightProbe 诊断工作台交付说明

本目录是一版可运行的光照探针 / GI 诊断实现。样本由固定种子 `424242` 的内置确定性生成器产出，不访问网络目标，也不依赖容器。

## 快速运行

```bash
go mod tidy
go test -count=1 ./...
go run ./cmd/diagnostic --workdir artifacts/acceptance
```

验收 CLI 会生成：

- `artifacts/acceptance/scene.json`：确定性场景、定点 SH 与解析真值。
- `artifacts/acceptance/report.json`：机器可读验收报告。
- `artifacts/acceptance/verification-report.md`：Markdown 验收表。
- `artifacts/acceptance/lightprobe.db`：SQLite 房间检查点。

浏览器回退界面：

```bash
go run . -http=127.0.0.1:18099
# 打开 http://127.0.0.1:18099
```

Wails 桌面构建：

```bash
wails build -tags wails -s -m
# Windows 交叉编译：
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails -o lightprobe.exe .
```

Wails 入口使用 `wails` build tag，因此默认 `go test ./...` 不要求 Linux GTK/WebKit 开发包；带 tag 的入口才导入 Wails。`frontend/dist` 是免 Node 打包步骤的原生 HTML/CSS/JS。

## 结构

- `internal/diag/errors.go`：`E_SCHEMA`、`E_GEOM`、`E_RANGE`、`E_STATE`、`E_VER`、`E_DIFF` 等结构化错误，携带房间、格点和字段。
- `internal/scene/`：场景 schema、固定种子生成器、解析真值、AABB/三线性解析公式和结构校验。
- `internal/probe/engine.go`：内部探针场、格点索引和显式依赖图装配。
- `internal/probe/scheduler.go`：固定工作线程、每线程探针缓冲、加权 SH 临时缓冲预算、阻塞排队事件和顺序提交。
- `internal/probe/reconstruct.go`：定点 SH 重建和逐通道辐亮度重建。
- `internal/probe/interpolate.go`：三线性插值、边界邻房映射与跨域线性半混合。
- `internal/probe/graph.go`：依赖图、前向失效闭包和拓扑排序。
- `internal/probe/mutate.go`：格点编辑、房间分辨率调整和失效清单。
- `internal/probe/state.go` / `retry.go`：房间状态机、增量调度、单格点/房间重试和像素拓扑重算。
- `internal/probe/persistence.go`：房间提交、检查点恢复和进度。
- `internal/checkpoint/store.go`：纯 Go `modernc.org/sqlite`，事务、互斥串行写入、payload/全局输入哈希/版本校验。
- `internal/probe/compare.go` / `verify.go`：快照逐项比较、首个 `E_DIFF` 定位和真值精度核验。
- `internal/acceptance/acceptance.go`：一条命令覆盖全部验收路径。
- `app.go` / `main_wails.go` / `frontend/dist/`：Wails 绑定与诊断界面。

## 关键实现约定

样本包含 3 个互不重叠、仅表面接触的房间，共 42 个探针格点和 4 个屏幕像素。解析辐亮度是世界坐标仿射函数，因此三线性结果只有浮点舍入误差。

边界规则为 `linear_half_mix`：在声明混合宽度内，

```text
t = clamp(distance / width, 0, 1)
alpha = 0.5 * (1 - t)
blended = (1-alpha) * L_owner + alpha * L_adjacent
```

精确失效依赖链为：

```text
source:radiance:<probe>
  -> recon:<probe>
  -> blend:<query/boundary-anchor>
  -> interp:<query>
  -> pixel:<pixel>
```

分辨率调整会重建该房间的格点拓扑；其他房间记录按 ID 和坐标原样复用。

房间状态迁移：

```text
pending -> sampling -> reconstructed -> committed
                     \-> failed -> sampling
reconstructed -> sampling（增量重算）
```

已提交房间的格点、SH 和混合参数冻结；直接编辑返回 `E_VER` 并给出冲突房间。

SQLite 每个房间在一个事务中写入重建 payload、payload hash、SH 摘要、依赖集合、输入 hash、采样参数版本和算法版本。重启时全局 meta 或任一房间 hash/版本不匹配，`OpenValidOrReset` 会删除旧库并以空快照全量重建，避免部分混用。

## 实际验证结果

以下结果来自当前工作区：

- `go version`：`go1.22.12 linux/amd64`；`go.mod` 声明 `go 1.22.0` / `toolchain go1.22.12`。
- `go test -count=1 ./...`：全部包通过。
- `go test -race -count=1 ./...`：通过。
- `go test -count=20 -run TestRunBoundedJobs ./internal/probe`：预算排队测试稳定通过。
- `go run ./cmd/diagnostic --workdir artifacts/acceptance`：29/29 检查项 PASS。
- 真值最大误差：探针辐亮度 `0`，像素着色 `2.220446049250313e-16`，边界跳变 `1.1102230246251565e-16`；声明容差为 `1e-8`。
- 单线程与四线程结果：SH 定点系数、辐亮度位模式和格点提交 ID 序列一致。
- 增量编辑 `probe:room-c:0:0:0`：1 个格点重算、41 个格点缓存命中、仅 `pixel-ac-boundary` 重算。
- 分辨率 `room-c -> 3×3×2`：18 个 C 房间格点重建，A/B 房间 30 个格点缓存命中，增量与空缓存全量一致。
- 检查点恢复：恢复 `room-a`、`room-b`，重建 `room-c`，房间提交序列为 `room-a,room-b,room-c`；错误 hash 被识别并整体重置。
- 单格点注入异常：仅目标格点失败，其他格点保持 reconstructed；重试后快照与基线逐位一致。
- 前端脚本：用户态 Node `v20.19.5` 执行 `node --check frontend/dist/app.js` 通过。
- Windows：`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails ...` 通过；`GOOS=windows go env GOOS` 输出 `windows`。
- 浏览器回退：实际 POST `EditProbe` 返回 `jobs=1 cache_hits=41`，失效节点数 5，像素范围仅 `pixel-ac-boundary`。

## 当前容器中的 Wails 限制

`wails version` 为 `v2.10.2`。加入用户态 Node 后，`wails doctor` 可识别 `npm 10.8.2`，但当前 Linux 容器没有免密 sudo，且缺少 `pkg-config` 可执行文件与 `libwebkit2gtk-4.0-dev`，所以 Linux 桌面打包命令：

```bash
wails build -tags wails -s -m
```

会停在 `pkg-config ... webkit2gtk`。在 Ubuntu 上补齐：

```bash
sudo apt install pkg-config libgtk-3-dev libwebkit2gtk-4.0-dev nodejs npm
wails doctor
wails build -tags wails
```

Windows 10/11 上安装 Go 1.22+、Node 18+ 和 Wails CLI 后，运行 `go mod tidy`、`wails doctor`，再执行 `wails build -tags wails` 即可；WebView2 通常由 Wails/安装器策略处理。当前已在 Linux 上完成 Windows target 的 Go 交叉编译验证，但没有实际 Windows 桌面会话。
