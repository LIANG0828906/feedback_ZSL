# HDR Proof Workbench

面向游戏直播与互动媒体分发的 HDR→SDR 合规输出管线。项目包含确定性样本生成、结构校验、转换与逐帧自证、路/分段状态机、SQLite 原子检查点、异常注入、三路径一致性比对、独立审计、CLI 与 Wails 桌面工作台。

核心实现只依赖 Go 标准库与纯 Go SQLite 驱动 `modernc.org/sqlite`。样本、检查点、日志和导出均落在本地，不需要账号、GPU、数据库服务或运行时网络。

## 已交付能力

- 4 路确定性样本：32×24/24×16/16×16/20×12，共 95 帧、18 段。
- 两种输入表达：`linear_float32_le` 与 `rgb10le`。
- 三种输入色域：BT.2020、Display-P3、BT.709；峰值覆盖 600/1000/1500/4000 nits，帧率覆盖 24/30/50/60 fps。
- 逐帧真值：目标色域 BT.709 中 4 个色块的期望线性 RGB、峰值亮度、色域边界坐标。
- 结构校验：尺寸、通道、编码、峰值、帧率、色彩空间、文件头、CRC32、时间戳单调与真值对齐。
- HDR→SDR：曝光、源色域→BT.709 矩阵、ACES/Reinhard 色调映射、色域 desaturate/clip、8 位 sRGB 输出。
- 逐帧自证：输入声明、参数、转换链摘要、输出统计、色块真值偏差、记录哈希。
- 路/分段状态机：非法迁移返回 `E_STATE` 且保持原状态；已提交分段不可覆盖。
- 外部 JSON 注入：坏帧、声明矛盾、参数越界、分段内部失败，分别进入 `E_FRAME`、`E_META`、`E_PARA`、`E_SEG`。
- SQLite 原子提交：帧与分段元数据同事务写入；版本/内容哈希失配时整体作废并全量重算。
- 并发边界：路级工作池、最大并行度、每线程有界帧缓冲；耗尽时阻塞并记录 `frame_buffer_full` 事件。
- 三路径比对：实时、断点恢复、全量复算逐像素、逐 proof 字节一致；差异返回 `E_DIFF` 并定位首个路/帧/字段/分段。
- 独立审计：只读取 SQLite proof、持久化像素、样本清单/真值与指纹，不调用转换链；缺失/重复记录返回 `E_AUDIT`。
- 桌面工作台：多路/分段状态、HDR/SDR 对照、真值偏差、proof、审计/比对、参数重算、未提交段重试与导出。

## 目录

```text
cmd/hdrproof/             CLI
internal/apperr/          稳定错误码与上下文字段
internal/sample/          HDRSEQ01 读写、生成器、校验
internal/color/           线性光矩阵、传递函数、sRGB 编解码
internal/convert/         转换链与逐帧自证
internal/state/           路/分段状态迁移
internal/engine/          并行执行、注入、重试、原子提交编排
internal/snapshot/        SQLite schema、版本识别、哈希校验
internal/compare/         快照逐帧/逐条差异定位
internal/audit/           独立只读审计
internal/exporter/        SDR/HDR 预览、proof 与索引导出
internal/verify/          可复现验收矩阵
desktop/                  Wails 桌面模块（独立 Go module）
artifacts/demo/           已生成的样本、数据库、日志与验收报告
docs/                     架构与验收说明
scripts/                  Windows/本地验证脚本
```

## 快速验证

环境要求：Go 1.22+。本仓库实际验证使用 `go1.22.12`，`GOPROXY=https://proxy.golang.org,direct`。

```bash
go version
go env GOFLAGS GOPROXY

go test ./...
go test -race ./...
go vet ./...

go run ./cmd/hdrproof demo --root artifacts/demo --workers 4 --buffer 2
```

`demo` 会重建样本并执行：

1. 实时处理；
2. 提交 5 个分段后取消，再从已持久化分段恢复；
3. 一次性全量复算；
4. 三组逐帧/逐条比对；
5. 独立审计；
6. 四类异常注入与失败分段重试。

机器可读结果见 `artifacts/demo/report.json`，事件日志见 `artifacts/demo/demo.log`。

## CLI

```bash
# 生成样本
go run ./cmd/hdrproof generate --out artifacts/demo/samples --seed 42

# 实时/恢复/全量
go run ./cmd/hdrproof run \
  --samples artifacts/demo/samples \
  --db artifacts/demo/full.db \
  --mode full \
  --target-peak 100 --exposure 0 --tone-map aces --gamut desaturate \
  --workers 4 --buffer 2

# 比较两个快照
go run ./cmd/hdrproof compare \
  --left artifacts/demo/realtime.db \
  --right artifacts/demo/full.db

# 独立审计
go run ./cmd/hdrproof audit \
  --samples artifacts/demo/samples \
  --db artifacts/demo/full.db

# 导出 SDR、HDR 预览、proof 与 index
go run ./cmd/hdrproof export \
  --db artifacts/demo/full.db \
  --samples artifacts/demo/samples \
  --out artifacts/demo/export
```

注入运行示例：

```bash
go run ./cmd/hdrproof run \
  --samples artifacts/demo/samples \
  --db artifacts/demo/injection-bad_frame.db \
  --mode full \
  --inject artifacts/demo/samples/injections/bad_frame.json
```

参数更新计划通过外部 JSON 提供，作用于未提交分段：

```json
[
  {
    "route_id": "arena-linear-bt2020",
    "from_segment": 2,
    "params": {
      "target_peak_nits": 150,
      "exposure_ev": 0,
      "tone_map": "aces",
      "gamut_mapping": "desaturate",
      "output_bits": 8
    }
  }
]
```

```bash
go run ./cmd/hdrproof run --samples artifacts/demo/samples \
  --db artifacts/demo/full.db --mode full \
  --param-schedule docs/sample-param-schedule.json
```

## 错误码

| 错误码 | 含义 | 必需上下文 |
|---|---|---|
| `E_DECODE` | 样本容器、CRC、载荷或数值损坏 | route/frame/field |
| `E_META` | 声明矛盾、缺失或真值不对齐 | route/segment/field/expected |
| `E_PARA` | 参数越界或不支持 | route/segment/field/expected |
| `E_STATE` | 非法路/分段迁移 | route/segment/field |
| `E_FRAME` | 受控坏帧注入 | route/segment/frame/field |
| `E_SEG` | 分段内部失败 | route/segment/field |
| `E_DIFF` | 三路径或快照首个差异 | route/frame/field/segment |
| `E_AUDIT` | 独立复核失败、record 缺失/重复/篡改 | route/frame/field |

## 确定性与哈希

- 参数版本：`SHA-256(canonical JSON(params))`。
- 算法版本：`hdr2sdr-2026.11.1`。
- 记录哈希：去掉 `record_sha256` 后对 proof JSON 做规范化 SHA-256，再写回字段。
- 内容哈希：覆盖分段计划、输入/输出/自证摘要、参数/算法/真值版本。
- SQLite `commit_order = route_order × 1,000,000 + segment_index`，写入由单连接与互斥锁串行化。
- 任一版本、像素 hash、proof hash、分段内容 hash 或计划不一致时，整库快照作废并全量重算。

## Wails 桌面

桌面端位于 `desktop/`，使用静态 HTML/CSS/JS，不需要 Node 或 npm。Windows 上：

```powershell
go version
wails doctor

Push-Location desktop
go test ./internal/workbench
wails dev 2>&1 | Tee-Object -FilePath ..\artifacts\wails-dev.log
```

构建 Windows 桌面产物：

```powershell
Push-Location desktop
wails build -platform windows/amd64 -skipbindings
```

当前 Linux 环境已完成 `GOOS=windows CGO_ENABLED=0 go build` 与完整 `wails build -platform windows/amd64 -skipbindings`，均成功。Linux 的 `wails doctor` 报告缺少 `libwebkit`，因此本机未启动 GUI；Windows 运行时验证脚本见 `scripts/verify-windows.ps1`。

## 实际验证摘要

最终验收时间：`2026-11-24T02:36:45Z`（UTC）。

- 样本真值指纹：`26a1a5e4e3302d05e1491eb2e14951adcad0e4a89a8b95c5a89799f77756e263`
- 实时：95 帧、95 条 proof、18/18 段、4/4 路。
- 断点中断：5 段、28 帧已持久化；恢复完成后 18/18 段，且复用匹配快照。
- 全量：95 帧、95 条 proof、18/18 段。
- 三组比对：全部相等，95 帧。
- 独立审计：4/4 路、18/18 段、95/95 帧与记录通过。
- 四类注入：`E_FRAME`、`E_META`、`E_PARA`、`E_SEG` 全部命中；失败分段重试通过。
- 帧缓冲阻塞总计 110 次（实时 35、中断恢复 20、继续恢复 22、全量 33）。
- 根模块 `go test ./...`、`go test -race ./...`、`go vet ./...` 通过。
- 桌面业务层 `go test ./internal/workbench` 通过。
- Windows Wails 构建成功：`desktop/build/bin/hdrproof-workbench.exe`。

更详细的架构与验收记录见 `docs/ARCHITECTURE.md` 和 `docs/ACCEPTANCE.md`。
