# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

```text
go version go1.22.12 linux/amd64
Wails CLI v2.10.2
CGO_ENABLED=0（核心与 Windows Wails 交叉构建均使用纯 Go SQLite 驱动）
```

`wails doctor` 探测到当前 WSL 为 Ubuntu 26.04，但缺少或无法调用 `pkg-config` / `libgtk-3-dev` / `libwebkit2gtk-4.0-dev` 主机构建链；因此未声称在该 WSL 中启动 Linux WebKit 窗口。

已通过 WSL 的 `reg.exe` 互操作确认 Windows WebView2 Runtime：

```text
HKLM\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}
pv    REG_SZ    158.0.4370.33
```

Windows 图形会话仍建议在 Windows 主机运行一次 `wails doctor` / `wails dev` 做最终人工确认。

## 1. 全量测试

命令：

```bash
CGO_ENABLED=0 go test -count=1 ./...
```

实际结果：

```text
ok  storyboardstudio/internal/api
ok  storyboardstudio/internal/checkpoint
ok  storyboardstudio/internal/interop
ok  storyboardstudio/internal/pool
ok  storyboardstudio/internal/workbench
```

覆盖的关键测试：

- 三份固定种子样本全部通过真值、确定性、断点续演和三路径一致性核验。
- 时长编辑只产生末尾镜头 cache miss，其余已提交镜头命中 SQLite 持久缓存。
- 前置条件编辑传播到因果下游、汇聚节点和互斥对端，但不污染无关上游。
- 内容哈希或算法版本不匹配时删除 checkpoint 快照。
- v1/v2/v3 升级痕迹、默认值、`E_LOSSY` 和重复导出字节稳定性。
- worker 池最大并行度不超过 2，满缓冲时第三次写入阻塞且最终不丢记录。
- HTTP API 的运行、编辑、导出路径。

`CGO_ENABLED=0 go vet ./...` 无输出并以退出码 0 完成。

## 2. 本地样本与业务验收

生成样本：

```bash
CGO_ENABLED=0 go run ./cmd/storyboard generate --out samples
```

产出：

```text
samples/project-252001.json
samples/project-252002.json
samples/project-252003.json
```

执行：

```bash
CGO_ENABLED=0 go run ./cmd/storyboard verify --samples samples
```

实际结果摘要：

```text
narrative-seed-252001 shots=8 interrupt=3 ok=true
  truth MATCH
  determinism MATCH
  resume-suffix MATCH
  checkpoint-path MATCH
  three-paths MATCH
  edit-0/edit-1/edit-2 MATCH

narrative-seed-252002 shots=7 interrupt=2 ok=true
  truth MATCH / determinism MATCH / resume-suffix MATCH
  checkpoint-path MATCH / three-paths MATCH / edits MATCH

narrative-seed-252003 shots=8 interrupt=3 ok=true
  truth MATCH / determinism MATCH / resume-suffix MATCH
  checkpoint-path MATCH / three-paths MATCH / edits MATCH
```

这表示：

- 逐镜采样点与生成器真值在声明容差内一致。
- 不中断推进、检查点续演、编辑后重演的镜头顺序、采样点、终态标志和事件到达顺序逐项相等。
- 从第 `interrupt` 个已提交镜头后续演得到的剩余镜头，与全量推进后缀逐镜对齐。
- 若出现不一致，比较器会返回首个镜头序号、字段名、左侧值和右侧值。

## 3. 负向异常码

命令：

```bash
CGO_ENABLED=0 go run ./cmd/storyboard negative-checks
```

实际摘要：

```text
missing-schema-field: E_SCHEMA ok=True
dangling-reference: E_REF ok=True
directed-cycle: E_REF ok=True
unknown-choice-option: E_CHOICE ok=True
unsupported-storyboard-version: E_VERSION ok=True
negative-duration: E_SCHEMA ok=True
illegal-aspect-path: E_SCHEMA ok=True
```

悬空引用与成环用例均返回首个冲突节点 `n00_world`。

## 4. HTTP 工作台冒烟

实际启动：

```bash
CGO_ENABLED=0 go build -o /tmp/storyboard-check ./cmd/storyboard
/tmp/storyboard-check serve --addr 127.0.0.1:18788 \
  --samples "$PWD/samples" --db /tmp/sb-data-2/test.db
```

已验证：

- `GET /api/health` 返回 `ok:true` 和 `deterministic-engine/v1.1.0`。
- `GET /api/projects` 返回三份工程。
- `GET /` 返回工作台 HTML。
- `POST /api/projects/narrative-seed-252002/verify` 返回 `shot_count=7, interrupt_at=2, ok=true`。
- 编辑末镜时长返回 `cache_hits=[0,1,2,3,4,5]`、`cache_misses=[6]`、`comparison.ok=true`。
- 在第 3 镜后中断返回 `interrupted=true, shots=4`；续演后恢复为完整 8 镜，并输出正确终态标志与事件顺序。

## 5. Wails / Windows 构建

命令：

```bash
CGO_ENABLED=0 wails build \
  -tags wails_desktop -s -skipbindings -nopackage \
  -platform windows/amd64
```

实际结果：

```text
Frontend Directory | .../web
Tags               | [wails_desktop]
• Compiling application: Done.
Built '.../build/bin/NarrativeStoryboardStudio.exe'
```

该结果验证 Wails 入口和绑定代码可编译为 Windows amd64 目标。Windows WebView2 Runtime 的图形启动需在 Windows 环境执行 `wails doctor` 与 `wails dev` / `wails build` 做最终确认。
