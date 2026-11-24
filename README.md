# 游戏录屏素材内容完整性核验工具

这是一个只使用 Go 标准库实现的本地桌面核验工具。它按固定种子生成可复现的录屏帧序列和解析真值，完成分块内容哈希、时间轴连续性检测、残差/噪声/图元外接框复核、替换区间定位、分段提交、检查点续跑和本地 HTTP 工作台。

目标环境是 Windows 本机 + Go 1.22 或更高版本；不依赖第三方图形/音视频库、数据库、网络、账号、Docker 或容器。

## 快速开始（Windows PowerShell）

```powershell
go version
go mod init gameintegrity   # 仓库已包含 go.mod，新建副本时才需要
.\scripts\demo.ps1
.\scripts\serve.ps1
```

打开 `http://127.0.0.1:8090` 即可使用工作台。也可手动执行：

```powershell
go test ./...
go vet ./...
go run ./cmd/verifytool generate -out data/samples -seed 20261124
go run ./cmd/verifytool demo -data data/samples -out artifacts/demo-results.json
go run ./cmd/verifytool serve -data data/samples -addr 127.0.0.1:8090
```

Linux/WSL 对应脚本为 `scripts/demo.sh`。

## CLI

```text
verifytool generate -out data/samples -seed 20261124
verifytool verify   -data data/samples -sample small
                    [-mode auto|fine|candidate]
                    [-path full|realtime|resume]
                    [-work artifacts/work/small]
                    [-budget 24] [-block 8]
                    [-residual 0.03] [-noise 0.03]
                    [-history 3] [-workers 3] [-buffer-mb 64]
                    [-json-out report.json]
verifytool demo     -data data/samples -out artifacts/demo-results.json
verifytool compare  -left full.json -right realtime.json
verifytool serve    -data data/samples -addr 127.0.0.1:8090
```

`verify` 的 `-path` 对应三条核验路径：

- `full`：一次性全量核验，不读写检查点。
- `realtime`：按段核验并在每段提交后原子持久化。
- `resume`：读取版本匹配的已提交分段，只继续剩余分段；版本失配时整份旧快照作废并全量重算。

`compare` 对两份报告做逐帧严格比较；首个不一致会返回 `E_DIFF`，包含帧号、分块坐标、字段名和左右两侧取值。

## 确定性样本

`generate` 会重建 `data/samples`，固定种子 `20261124` 生成以下样本：

| 样本 | 分辨率 | 帧数 | 分块 | 替换真值 | 用途 |
|---|---:|---:|---:|---|---|
| `tiny` | 80x64 | 16 | 8 | 6..8 | 自动选择精细模式 |
| `small` | 96x192 | 36 | 8 | 18..24 | 候选优先模式 |
| `medium` | 240x192 | 90 | 16 | 36..46 | 三路径与分段续跑 |
| `large` | 480x360 | 160 | 24 | 71..86 | 大分辨率降采样取舍 |
| `small-e-order` | 96x192 | 36 | 8 | 18..24 | 注入时间戳倒置 |
| `small-e-mark` | 96x192 | 36 | 8 | 18..24 | 注入水印缺失 |
| `small-e-frame` | 96x192 | 36 | 8 | 18..24 | 注入坏帧 |
| `small-e-size` | 96x192 | 36 | 8 | 18..24 | 注入帧头尺寸突变 |

每个样本目录包含：

```text
manifest.json                 帧宽高、通道、帧数、时间戳、分块、分段、异常注入
truth.json                    水印位置、每帧图元外接框、替换区间、基准帧路径
frames/frame-000000.rgbx      RGBX 帧容器：20 字节头 + RGB24 原始像素
baseline/frame-000000.rgb     原始基准像素（无 RGBX 头）
```

`RGBX` 头字段为：magic `RGBX`、版本、宽、高、通道、flags、帧号、payload 长度。坏帧通过 flags 标记，尺寸突变通过头部字段标记，因此能与“像素数据截断”的 `E_DECODE` 区分。

合成画面由固定渐变背景、矩形/圆形/三角形、16-bit 离散帧号水印和局部亮度扰动构成。替换区间使用独立的确定性噪声画面，但保留正确水印，以检验内容异常而非结构异常。

## 核验链

1. **加载与结构校验**：验证清单/真值、时间戳、帧号、帧头宽高通道、payload 长度、基准像素大小、水印和分块参数。
2. **块级内容哈希**：每帧按配置分块计算 SHA-256；块哈希写入坐标后，按 `(row,col)` 排序汇总为帧摘要。处理顺序和 worker 标识不参与摘要，单像素变化会改变所属块哈希。
3. **时间轴连续性**：比较相邻帧块哈希，统计变化块比例；超过 `hash_jump_ratio` 的帧成为候选。水印值必须与帧号严格单调一致。
4. **候选复核**：候选帧及其相邻帧执行与前/后若干帧的残差能量、块级噪声方差均值与离散度、图元外接框偏移计算。判定使用 `max(noise_mean, noise_dispersion)`，因此既输出离散度，也能稳定区分平滑背景与高噪声替换帧。
5. **逐帧判定**：输出 `intact`、`suspect`、`replaced`、`invalid`、置信度、异常分块坐标和原因。连续 `replaced` 帧形成替换区间，并在 `tolerance_frames` 内与真值比较。

默认参数：`block_size` 取样本声明值，残差阈值 `0.03`，噪声阈值 `0.03`，外接框偏移阈值 `2` 像素，哈希跳变比例 `0.5`，历史窗口 `3`，采样预算 `24`，最大 worker 数 `3`，像素缓冲预算 `64 MiB`。

阈值语义是“归一化指标 >= 阈值即异常”。因此提高阈值（收紧判定）时，异常块数量只可能不变或减少。阈值越界返回 `E_PARA` 并指出字段名与可用范围。

## 规模自适应策略

取舍函数只依赖声明的单帧像素数、帧数、采样预算和用户模式：

```text
frame_units      = ceil(frame_pixels / 100000)
effective_frames = frame_count * frame_units
if effective_frames <= sampling_budget: fine
else: candidate-priority
stride           = ceil(effective_frames / sampling_budget)
block_downsample = 2 if frame_pixels > 100000 else 1
```

`fine` 模式对全部帧做完整复核；`candidate-priority` 仍对全部帧计算合同要求的精确块哈希，但只对哈希跳变候选及其邻域执行昂贵的残差、噪声和外接框复核。因此规模策略节省的是精细复核成本，不会用近似摘要替代逐帧块哈希。报告记录 `stride`、`block_downsample`、跳过的帧区间、候选帧、实际精细帧、判定依据、worker 数和像素缓冲槽位/等待次数。

`scale.CheckIntervalAgreement` 比较两种模式的替换区间；起止偏差超过声明容差时返回 `E_SCALE`，`left`/`right` 同时包含两侧定位结果。

## 分段状态机与检查点

固定时长分段，状态只允许：

```text
pending -> verifying -> committed
                    \-> failed -> verifying
```

`committed` 是终态。非法迁移返回 `E_STATE` 且原状态不变。已提交结果深拷贝冻结，进度为“已提交分段数 / 总分段数”。

每段提交写一个 `segments/000000.json`：

1. 在目标目录创建临时文件；
2. 写入该段帧摘要、块指标、残差、噪声、外接框偏移、完整性判定、内容哈希、参数版本和算法版本；
3. `Sync` 后关闭，再原子 `rename` 为最终序号文件；
4. 提交序号必须从 0 连续递增，写入由目录级互斥锁串行化。

重启时读取连续分段。内容哈希、参数版本或算法版本任一不匹配时，整个 `segments` 目录移动到 `invalid/snapshot-<时间>`，不允许部分混用；报告输出 `checkpoint_invalidation.fields`、`expected`、`actual` 和原因，然后全量重算。

## 错误码

| 错误码 | 含义 | 关键字段 |
|---|---|---|
| `E_DECODE` | 清单/真值缺失、JSON 损坏、帧或基准像素截断 | `frame`, `field` |
| `E_META` | 宽、高、通道、帧数、帧头元数据不匹配 | `field`, `expected`, `actual` |
| `E_PARA` | 分块、阈值、预算、缓冲或模式参数非法 | `field`, `available_range` |
| `E_SCALE` | 精细/候选模式替换区间偏差超容差 | `left`, `right` |
| `E_STATE` | 非法分段迁移、重复/乱序提交 | `field`, `details` |
| `E_DIFF` | 三路径或两份报告逐帧不一致 | `frame`, `block`, `field`, `left`, `right` |
| `E_ORDER` | 时间戳倒置或帧号水印非单调 | `frame`, `block` |
| `E_MARK` | 水印边框/bit cell 缺失或值错误 | `frame`, `block` |
| `E_FRAME` | 帧被标记为坏帧 | `frame`, `block` |

## 三路径一致性

三条路径使用同一块哈希、同一候选检测、同一阈值和同一判定函数。`realtime` 每段提交，`resume` 复用版本匹配的已提交段，`full` 不读检查点。`engine.Compare` 按帧比较摘要、判定、残差、噪声、外接框偏移、内容哈希和每个块哈希；第一个差异会以 `E_DIFF` 返回，不会用候选模式的近似结论替代精细比较。

## 工作台

本地 HTTP 工作台提供：

- 帧时间轴、分段状态、提交进度；
- 原帧、相邻帧残差热力图、块级噪声方差示意图；
- 替换区间、真值、逐帧判定/置信度/指标/异常块；
- 逐段查看、开始/续跑、全量重算、暂停、继续、取消、重试、JSON 导出；
- 自动轮询异步任务状态。

主要 API：

```text
GET  /api/samples
GET  /api/samples/{id}
POST /api/samples/{id}/verify?mode=auto&budget=24&block=8&fresh=1
GET  /api/samples/{id}/job
POST /api/samples/{id}/pause|resume|cancel|retry
GET  /api/samples/{id}/segment/{index}
GET  /api/samples/{id}/frame/{n}.png
GET  /api/samples/{id}/residual/{n}.png
GET  /api/samples/{id}/noise/{n}.png
GET  /api/samples/{id}/export
```

工作线程池限制最大并行度；每个复核任务通过阻塞式像素缓冲信号量申请“当前帧 + 历史邻居”所需的槽位。缓冲耗尽时排队等待并记录 `pixel_buffer_waits`，不会静默丢帧。强制终止后，只有已原子提交且版本匹配的分段会被续跑使用。

## 验证

仓库中的 `artifacts/demo-results.json` 是 Linux/WSL 实际运行结果，`artifacts/demo-results-windows.json` 是 Windows 原生实际运行结果，`artifacts/TEST_RESULTS.md` 记录环境、命令、测试和验收输出。重新生成：

```powershell
.\scripts\demo.ps1
```

`demo` 自动执行：

- 四个正常样本的自动模式、精细模式、候选模式；
- 替换区间与真值/容差比较；
- medium 的 full/realtime/resume 三路径逐帧比较；
- 参数版本失配的整份检查点作废检查；
- 四类注入异常码、帧号和分块坐标。

## 目录

```text
cmd/verifytool/          CLI、demo、serve
internal/apperr/         统一错误信封
internal/model/          数据模型、配置、区块几何
internal/sample/         确定性生成器与异常注入
internal/frameio/        RGBX 读取、结构校验、水印、内容哈希
internal/hashx/          块哈希与顺序无关帧摘要
internal/analyze/        残差、噪声、外接框、判定
internal/scale/          规模自适应与 E_SCALE
internal/segment/        分段状态机与冻结
internal/checkpoint/     原子分段检查点与整份作废
internal/engine/         核验编排、三路径、严格比较
internal/workpool/       并行 worker 与阻塞缓冲池
internal/workbench/      net/http 工作台与页面
scripts/                 Windows/Linux 复现脚本
artifacts/               实际运行记录（生成数据被 .gitignore 排除）
```
