# vcomp

`vcomp` 是一套面向虚拟制片实时合成的桌面层栈合成与验证工具。它从固定种子生成本地三层样本，把三种前景描述版本统一成内部有序层栈，执行确定性 alpha 合成与 sRGB 线性化，并通过 SQLite 检查点支持实时增量重算、取消恢复和一次性全量重算。

## 快速开始

要求 Go 1.22+。SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`，不需要 CGO、容器或外部数据库。

```bash
# Linux / WSL
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -trimpath -o artifacts/vcomp ./cmd/vcomp

./artifacts/vcomp gen --out samples --seed 42
./artifacts/vcomp verify --data samples --work artifacts/verify
./artifacts/vcomp workbench --data samples --version v3 \
  --db artifacts/workbench.db --project workbench --demo --out artifacts/workbench
```

Windows PowerShell：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\windows-build-test.ps1
```

脚本执行 `go test ./...`、`go build`、样本生成、完整验收、工作台演示，并保留 `artifacts/windows/workbench.db` 供断点恢复。

## 关键能力

- **三版本统一加载**：V1 使用 z-index、布尔可见性、8 位 straight、微秒；V2 使用显式有序栈、字符串可见性、8 位 straight、毫秒；V3 使用 `above` 引用链、整数可见性、16 位 premultiplied、tick 时间。V3 也兼容 8 位文档。
- **确定性合成**：内部固定自底向上的层顺序、8 位 straight sRGB RGBA 和纳秒帧时间基准。逐层在 linear sRGB 中执行 source-over，最后回到 sRGB 输出。
- **状态机与提交边界**：窗口只有 pending/compositing/committed/failed；合法迁移之外返回 `E_STATE`。提交时冻结源版本、层参数、时间基准和参数版本。
- **精确增量**：依赖图显式记录层 → 帧窗口 → 导出阶段。隐藏层或未出现层的变化不会使无关窗口失效；未失效窗口复用 SQLite 缓存。
- **三条一致路径**：实时缓存、取消后重启恢复、空缓存全量重算使用同一合成核心，输出帧、采样统计和窗口提交序列逐项相等。
- **跨版本导出**：确定性 JSON 与 PNG 产物；V3→V1/V2 的预乘层和层级引用会生成带字段路径、原值与处置方式的 `E_LOSSY` 报告。
- **SQLite 整体快照**：单连接串行事务写入；每个窗口保存内容哈希、帧摘要、参数版本、源版本、算法版本和冻结快照。任一哈希或版本不一致会删除整个工程快照并从空缓存重算。
- **资源与异常隔离**：窗口工作线程池、每线程像素缓冲上限、可选总缓冲预算；排队原因写入运行结果。窗口 panic、注入错误或样本损坏只会使该窗口失败，重跑可恢复。
- **交互诊断**：工作台展示源版本、层栈、窗口状态矩阵、ASCII 合成预览、采样点、差异热力图 PNG，并支持编辑参数、切换目标格式、增量重算和导出。

## 目录

```text
cmd/vcomp/          CLI 与交互工作台
model/              内部模型、错误码、快照、内容哈希、自动对比
compose/            alpha 归一、sRGB 线性化、合成、统计、PNG、热力图
formats/            V1/V2/V3 加载器、指令加载、跨版本导出
sample/             固定种子样本、指令与真值生成
engine/             窗口状态机、依赖图、资源限流、并发调度、工作区
store/              modernc SQLite 检查点与整体版本校验
docs/               格式/持久化契约、实际验证记录
scripts/            Windows PowerShell 构建与演示
samples/            已生成的确定性样本
artifacts/          本地验证数据库、工作台输出与 Windows 构建产物
```

## CLI

### 生成样本

```bash
go run ./cmd/vcomp gen --out samples --seed 42
```

固定种子生成：

- `samples/v1/scene.json`
- `samples/v2/scene.json`
- `samples/v3/scene.json`
- `samples/commands.json`
- `samples/truth.json`
- `samples/manifest.json`

真值包含 6 个采样像素/帧、24 帧时间戳、每层首个/最后可见帧和帧内容哈希。

### 加载检查

```bash
go run ./cmd/vcomp load --scene samples/v3/scene.json --commands samples/commands.json
```

输出统一后的源版本、尺寸、通道数、帧时间基准、顺序表达和层参数。

### 运行合成

```bash
# 全量路径
go run ./cmd/vcomp run --data samples --version v1 --db artifacts/demo.db \
  --project demo --mode full --workers 3 --frames-out artifacts/frames

# 复用已提交窗口
go run ./cmd/vcomp run --data samples --version v1 --db artifacts/demo.db \
  --project demo --mode realtime

# 提交 2 个窗口后取消；重启时恢复
go run ./cmd/vcomp run --data samples --version v1 --db artifacts/resume.db \
  --project resume --cancel-after 2
go run ./cmd/vcomp run --data samples --version v1 --db artifacts/resume.db \
  --project resume --mode resume
```

`run` 输出窗口状态矩阵（`P` pending、`C` compositing、`K` committed、`F` failed）、已提交/总窗口数、提交序列、复用窗口、重算窗口和缓冲排队事件。

### 导出

```bash
go run ./cmd/vcomp export --data samples --version v3 --target v1 \
  --out artifacts/export-v3-to-v1
```

目录包含目标版本描述、逐帧 PNG 和无时间戳的确定性 `manifest.json`。降级警告直接列在命令输出中。

### 验收

```bash
go run ./cmd/vcomp verify --data samples --work artifacts/verify
```

验收覆盖三版本真值、规范层栈一致性、重复合成、错误码、状态机、全量/实时/恢复、取消持久化、增量失效与缓存复用、资源排队、窗口故障隔离、跨版本往返与缓存毒化。

## 工作台

```bash
go run ./cmd/vcomp workbench --data samples --version v3 \
  --db artifacts/workbench.db --project workbench --out artifacts/workbench
```

命令：

```text
stack                       层栈与源版本
matrix                      窗口状态矩阵
frame <n>                   合成预览和采样统计
diff                        与上一次运行比较，写差异热力图 PNG
opacity <layer> <0..1>      编辑透明度并增量重算
visible <layer> <0|1>       编辑可见性并增量重算
timebase <ns>               编辑帧时间基准
target <v1|v2|v3>           切换目标导出格式
recompute                   复用缓存重算
full                        丢弃缓存全量重算
export <v1|v2|v3>           导出描述、帧和 E_LOSSY 报告
compare                     对比上次/当前帧、统计与提交序列
quit
```

`--demo` 自动展示编辑 `prop` 不透明度、隐藏 `character`、差异热力图和导出结果。

## 设计说明

### Alpha 与颜色

输入 straight 样本在渲染时先解码 sRGB，再乘 alpha 得到线性预乘贡献。V3 的 16 位预乘值按同一 encoded-domain 约定编码；加载到内部模型时转回 8 位 straight。16 位中间表示保证任意 8 位 straight 值经 V1→V3 往返后逐字节恢复，同时内部缓存和输出通道保持统一。

合成使用标准 source-over：

```text
out.rgb = src.linear * src.a + dst.rgb * (1 - src.a)
out.a   = src.a + dst.a * (1 - src.a)
```

全部计算使用 `float64`，最终按固定规则量化到 8 位，不依赖线程、GOMAXPROCS 或完成顺序。

### 增量与缓存

每个窗口的内容哈希只覆盖实际渲染贡献：可见层、正不透明度、像素哈希、帧时间。隐藏层的参数变化不会扩大窗口失效范围，但会使描述导出阶段失效。编辑后，失效窗口在一个 SQLite 事务中删除；未失效窗口仍满足当前解析快照的内容哈希/参数版本，从而复用。

重启时，每个已存窗口都会重新验证自身版本、快照哈希、参数版本、内容哈希、帧摘要和帧哈希。任何一项不一致都会清除整个工程快照，禁止新旧窗口混合使用。取消或异常发生在批处理边界时，已提交窗口已原子持久化，下一次运行只补算缺失窗口。

### 资源与异常

- `--workers` 限制窗口并行度。
- `--buffer` 是每工作线程像素缓冲上限。
- `RunOptions.PixelBufferTotalBytes` 可设置共享总预算；不足时排队并产生 `Queued` 事件，不静默丢帧。
- worker panic 被恢复为该窗口 `E_DECODE` 失败。
- 失败窗口不写入检查点；同批后续窗口回到 pending，下一次运行可重试。

详细契约见 `docs/format-contract.md`。

## 验证状态

本机实际结果记录在 `docs/verification-linux.md`：

- `CGO_ENABLED=0 go test ./...` 全部通过。
- `go vet ./...` 无报告。
- `vcomp verify` 的 19 项检查全部通过。
- 工作台增量演示只重算窗口 `[1,2,3,4]`，复用 `[0,5]`，差异热力图非零。
- Windows `.exe` 与 6 个测试二进制已交叉构建成功；当前 WSL 宿主机没有可调用的 Windows Go/PowerShell/Wine，因此 Windows 运行时测试需在 Windows 主机执行 `scripts/windows-build-test.ps1`。

## 测试

```bash
CGO_ENABLED=0 go test ./...
```

覆盖包：

- `compose`：sRGB 往返、16 位预乘无损、确定性、E_PARA、热力图定位。
- `formats`：三版本归一、错误契约、导出确定性、E_LOSSY、往返。
- `sample`：固定种子文件确定性、三版本真值、可见性终点。
- `engine`：全量/实时/恢复、并发一致、取消、增量、缓存复用、故障隔离、缓冲排队、缓存毒化、状态机。
- `store`：检查点往返、整体哈希校验、错误版本拒绝。
- `model`：首个不一致窗口/像素定位、隐藏层哈希语义。
