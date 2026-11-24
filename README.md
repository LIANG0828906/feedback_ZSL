# 交互式叙事与分镜工作台

首个可运行版本：Go 1.22 确定性叙事引擎、节点级失效传播、SQLite 镜头边界检查点、三版本分镜描述互操作、原生 JavaScript 工作台，以及可在 Windows/WebView2 构建的 Wails v2 入口。

样本由固定种子本地生成，不依赖外部素材、网络资源或 Docker。SQLite 使用 `modernc.org/sqlite` 纯 Go 驱动，因此核心命令建议统一使用 `CGO_ENABLED=0`。

## 快速验证

```bash
go version          # 需要 go1.22+
make samples        # 生成 samples/project-25200{1,2,3}.json
make test           # 全量单元/集成测试
make verify         # 真值、断点续演、三路径一致性
make negative       # E_SCHEMA/E_REF/E_CHOICE/E_VERSION 负向用例
make serve          # http://127.0.0.1:8787
```

也可以直接运行：

```bash
CGO_ENABLED=0 go run ./cmd/storyboard verify --samples samples
CGO_ENABLED=0 go run ./cmd/storyboard run --file samples/project-252002.json
CGO_ENABLED=0 go run ./cmd/storyboard export --file samples/project-252002.json --node n00_world --version 1.0
```

## 已实现能力

- **加载与校验**：`internal/model/validate.go` 校验必填字段、引用、DAG、非负定点毫秒时长与 `aspect://WxH` 画幅路径。字段缺失返回 `E_SCHEMA`；悬空引用或成环返回 `E_REF`，并给出首个冲突节点。
- **确定性推进**：`internal/engine/engine.go` 使用排序后的 Kahn 拓扑序，依据 `all/any/none` 标志位、互斥节点和 `all/any` 汇聚策略推进；事件后果写入剧情标志与角色状态，按固定间隔生成逐镜采样点。
- **真值判定**：固定种子生成器在 `internal/sample/generator.go` 中构造终态标志、事件顺序、可达集合和逐镜样本；`internal/engine/compare.go` 输出首个不一致镜头序号、字段名、左右值。
- **依赖与失效**：`internal/dependency/graph.go` 显式建立因果边、汇聚边、互斥边、标志生产/消费边、时长到采样呈现边。编辑前置条件或后果时传播到因果/标志下游与互斥对端；编辑时长时标记该镜及受全局时间轴偏移影响的后续镜头。
- **持久化缓存**：未失效且内容哈希一致的镜头从 SQLite `shot_cache` 复用；失效重演结果和会话摘要通过事务原子替换。
- **检查点续演**：每个镜头边界在同一事务内写入进度、选择偏移、标志快照、节点终态摘要、镜头/工程哈希、结构版本和算法版本。SQLite 连接池限制为 1，另有互斥锁保证写串行。
- **快照作废**：重启加载时工程哈希、结构版本或算法版本不匹配，会删除该会话全部检查点/会话快照；工作台随后自动从起点全量重演，不混用旧镜头。
- **三路径一致**：连续推进、中断存档续演、编辑后增量重演由 `internal/workbench/verify.go` 逐项比较镜头顺序、采样点、终态标志、角色状态和事件到达顺序。
- **版本互操作**：`internal/interop/interop.go` 支持 `storyboard/1.0`、`storyboard/2.0`、`storyboard/3.0`。三版分别使用 decisecond、millisecond、microsecond 时长单位，以及相对路径、URN、canonical URI 画幅表达；低版本升级补默认值并写转换痕迹，导出低版本无法表达的字段时返回 `E_LOSSY` 清单。
- **并发边界**：`internal/pool/pool.go` 限制最大并行 worker；每个 worker 的状态缓冲有容量上限，满时 `Push` 阻塞等待串行 writer 排空，不丢弃状态。多样本并行核验入口为 `workbench.VerifyProjectsParallel`。
- **工作台界面**：`web/` 是无需 Node 构建步骤的原生 JS 界面，展示因果图、节点镜头、选择时间轴、分镜预览、存档列表和失效高亮；支持编辑前置条件/时长、连续运行、中断、续演、逐节点导出和一键核验。

## HTTP API

启动 `make serve` 后：

- `GET /api/projects`、`GET /api/projects/{id}`、`GET /api/projects/{id}/graph`
- `POST /api/projects/{id}/run`
- `POST /api/projects/{id}/checkpoint`
- `POST /api/projects/{id}/resume`
- `POST /api/projects/{id}/edit`
- `POST /api/projects/{id}/verify`
- `GET /api/projects/{id}/export?node_id=...&version=1.0|2.0|3.0`
- `GET /api/sessions`、`GET /api/sessions/{id}/checkpoints`

## Wails / Windows WebView2

本机已确认 Wails CLI：

```text
Wails CLI v2.10.2
```

当前 WSL 环境缺少 `libwebkit2gtk-4.0-dev` 与可用的 `pkg-config/gcc` 包装链，因此没有把 Linux 桌面壳作为运行验证路径；纯 Go 核心、Web UI 和 Windows 目标不受影响。已通过 Windows 注册表确认 WebView2 Runtime：

```text
HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}
pv = 158.0.4370.33
```

并已实际执行成功：

```bash
CGO_ENABLED=0 wails build \
  -tags wails_desktop -s -skipbindings -nopackage \
  -platform windows/amd64
```

在 Windows 上请确认 WebView2 Runtime 可用，然后运行：

```powershell
wails doctor
wails build -tags wails_desktop -s -skipbindings
```

Wails 入口是 `main_wails.go`；默认 `go run .` / `go build .` 使用 `main_web.go` 启动浏览器工作台，不要求 Linux 安装 WebKit。

## 目录说明

- `internal/model`：工程、镜头、真值、错误码与结构校验。
- `internal/sample`：固定种子确定性样本生成器。
- `internal/engine`：拓扑推进、采样、真值/结果比对。
- `internal/dependency`：节点依赖图与失效清单。
- `internal/checkpoint`：SQLite 检查点、镜头缓存、事务与版本校验。
- `internal/workbench`：编辑/缓存/续演/三路径核验编排。
- `internal/interop`：v1/v2/v3 分镜描述转换。
- `internal/pool`：有界 worker 与有界状态缓冲。
- `internal/api`：工作台 JSON API 和静态资源服务。
- `web`：原生 JavaScript 前端与嵌入资源。
- `cmd/storyboard`：`generate`、`verify`、`run`、`export`、`serve`、`negative-checks` CLI。

实际验证记录见 `docs/VERIFICATION.md`。
