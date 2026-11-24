# 游戏与互动媒体音效编解码质量评估管线

这是一个仅使用 Go 标准库构建的本地确定性音效编解码评估工具。项目从音频原始序列与有损重建序列出发，完成逐帧 PSNR、SSIM、噪声基底、量化误差传播、边界合并、受限并行调度、原子检查点、三路径一致性和独立验证。

## 快速开始（Windows）

要求 Go 1.22 或更高版本。以下命令使用 Windows 本地路径：

```powershell
$env:GOMODCACHE = (Join-Path $PWD ".gomodcache")
go env -w GOMODCACHE="$env:GOMODCACHE"

go test ./...
go build -o .\bin\audioeval.exe .\cmd\audioeval

.\bin\audioeval.exe demo --dir .\artifacts\demo
.\bin\audioeval.exe verify `
  --sample-dir .\artifacts\demo\samples\q3 `
  --run-dir .\artifacts\demo\runs\resume
```

也可直接运行一键验收脚本：

```powershell
.\scripts\verify.ps1 -OutputDir .\artifacts\demo
```

不访问网络，不使用数据库、Docker 或容器。模块没有外部依赖。

## 目录

```text
cmd/audioeval/        CLI：generate、run、verify、demo
internal/model/       清单、真值、报告、错误码、版本和指纹
internal/sample/      确定性样本生成、PCM 与真值加载校验
internal/codec/       整数 Q14 DCT、量化、逆变换和系数误差
internal/metrics/     定点窗口统计、PSNR、SSIM、噪声和误差上界
internal/engine/      状态机、受限线程池、检查点、主评估流程
internal/verify/      不导入评估主路径的独立验证器
internal/compare/     三路径逐字段/逐块/逐样本摘要比对
internal/report/      JSON 报告与 PNG 差异热力图
docs/                 原任务说明
scripts/verify.ps1    Windows 本地验收脚本
```

## 样本与真值

固定种子生成：

- 原始序列：整数低频包络、确定性高频纹理和嵌入的解析脉冲标记。
- 重建序列：逐帧整数 DCT，按质量档位量化系数，再逆变换并做确定性重叠相加。
- `manifest.json`：采样率、通道、帧长、帧移、帧数、质量档位、量化步长、整体哈希、逐帧哈希、真值指纹和输入内容哈希。
- `truth.json`：逐帧脉冲位置、期望 PSNR、量化噪声基底标准差和真值指纹。
- `original.i16`、`reconstruction.i16`：小端 `int16` 交错 PCM。

支持档位：

| 档位 | 量化步长 | 方向 |
|---|---:|---|
| Q1 | 128 | 最低质量 |
| Q2 | 64 | 提升 |
| Q3 | 32 | 中等 |
| Q4 | 16 | 较高 |
| Q5 | 8 | 最高质量 |

加载时，缺字段、非法几何参数、不支持档位或真值指纹不完整返回 `E_PARA`；PCM 长度、内容哈希或逐帧哈希损坏返回 `E_DECODE`，并给出首个违规帧号或字段名。

## 评估与定点规则

评估按 `window` 与 `window_hop` 生成窗口；窗口起点被唯一分配给包含它的块核心，避免重复累计。

- PSNR：样本误差 SSE 使用 `int64` 累加，`10*log10(32767^2*count/sse)` 最终向下截断为整数微 dB。
- SSIM：均值、方差和协方差由 `int64` 整型矩计算；最终比值仅在声明位置转为 `float64` 并向下截断为 Q16。
- 噪声基底：对原始、重建序列分别计算整数一阶差分 SSE；同时以块样本误差 SSE 计算量化残留标准差并与逐帧真值对照。
- 量化系数误差：以 Q14 记录每个频域系数的量化前/后误差；报告包含逐帧、逐通道误差数组和逆变换 SSE。
- 误差上界：只使用量化步长、帧长、帧移、窗口尺寸和真值声明的噪声基底。质量档位收紧时步长减小，PSNR 单调改善。

误差上界的核心形式为：

```text
quant_variance = q^2 * (2N - 1) / (12N^2)
L2_bound = ceil(sqrt(window * (quant_variance + noise_std^2))
                + q * max(0, N - hop) / (2N))
```

其中 `q` 是量化步长，`N` 是帧长，`hop` 是帧移。计算不读取真值中的期望 PSNR。

## 块状态、边带与预算

每帧固定划分为 `blocks_per_frame` 个核心块。每个块在 `align`、`metrics`、`propagation` 三个阶段分别经历：

```text
pending -> processing -> committed
                    \-> failed -> processing
```

非法迁移返回 `E_STATE` 且不改变状态。块一旦提交，其指标、误差统计、误差上界和重建摘要被冻结；参数版本更新只传播到未提交块。

默认边带宽度为 `window + window_hop - 1`，按块两侧可用范围截断。每个块的最小右边带是分配给该块的所有窗口中 `max(start+window-coreEnd)`。不足时返回 `E_BAND`，错误中带块坐标和最小所需边带。窗口统计和残留统计的合并只依赖排序后的块坐标，不依赖完成顺序。

调度器限制：

- 最大工作线程数；
- 每线程最大采样缓冲；
- 有界任务队列。

队列满时生产者在队列上阻塞，并记录 `queue_budget_full`、块坐标和等待毫秒数，不丢帧。完成结果先进入收集器，再按全局块序串行提交，因此并行与串行产生相同块序列、哈希和指标。

## 检查点与恢复

每个块完成时写入一个独立 `block-%09d.json`：

- 量化误差摘要与样本域误差；
- 原始/重建核心哈希；
- PSNR、SSIM、噪声统计与误差上界；
- 输入内容哈希、参数版本和算法版本。

写入流程为同目录临时文件、`Sync`、关闭、重命名到唯一最终文件，并由互斥锁串行化。`meta.json` 保存样本、几何、参数版本、算法版本和真值指纹。

恢复时若任一版本、内容哈希或指纹不匹配，或块序列有缺口/损坏，整个快照作废并全量重算，禁止混用旧块。取消或强制终止只保留已原子发布的块；此时只生成 `complete=false` 的 `partial-report.json`，不发布 `report.json`。恢复完成后才生成完整报告和热力图。

## 三路径与独立验证

演示命令生成主样本 Q3，并执行：

- `realtime`：4 工作线程、队列预算 1；
- `full`：单线程、无缓冲队列；
- `resume`：先在第 3 个新块提交后取消，再从快照续跑。

自动比对逐帧指标、逐块指标、误差上界、系数误差序列、边界路径、重建核心哈希和块提交序列，定位首个不一致的帧、块与字段。

独立验证器 `internal/verify` 不导入 `engine`、`metrics` 或 `codec`，只读取：

- 已提交的块文件；
- 快照元数据；
- 清单中的真值指纹；
- 质量报告（用于逐项对照）。

它自行复算窗口 PSNR/SSIM、块质量、逐帧边界合并、噪声统计、误差上界和误差路径，不调用评估主路径。真值噪声基底与期望 PSNR 已作为块检查点字段持久化。

## CLI

```powershell
# 生成样本
.\bin\audioeval.exe generate `
  --dir .\artifacts\sample-q3 `
  --seed 424242 `
  --quality Q3 `
  --frame-length 256 `
  --frame-hop 128 `
  --channels 2 `
  --frame-count 8

# 评估
.\bin\audioeval.exe run `
  --sample-dir .\artifacts\sample-q3 `
  --run-dir .\artifacts\run-q3 `
  --mode realtime `
  --window 64 `
  --window-hop 16 `
  --blocks-per-frame 4 `
  --threads 4 `
  --buffer-samples 8192 `
  --queue 1

# 断点续跑
.\bin\audioeval.exe run `
  --sample-dir .\artifacts\sample-q3 `
  --run-dir .\artifacts\run-q3 `
  --mode resume

# 独立验证
.\bin\audioeval.exe verify `
  --sample-dir .\artifacts\sample-q3 `
  --run-dir .\artifacts\run-q3
```

错误码：

- `E_PARA`：参数、字段、几何、档位或预算不可满足；
- `E_DECODE`：PCM 长度、哈希或序列内容损坏；
- `E_STATE`：非法状态迁移或检查点版本冲突；
- `E_BAND`：块边带不足，错误包含坐标与最小边带。

## 验证结果

实际运行结果记录见 `VALIDATION.md` 与 `artifacts/demo/validation.json`。当前演示为 8 帧、2 通道、32 个块，三路径全部一致，独立验证器通过，预算队列产生了实际阻塞事件，Q1/Q3/Q5 的 PSNR 严格单调改善。

## 实现说明

- DCT 为纯 Go 整数定点实现，复杂度 `O(N^2)`；当前面向确定性样本和验收规模，不追求大文件吞吐。
- 样本是解析生成的原始 PCM，不依赖真实音频文件或线上资源。
- 所有文件路径由 `filepath` 组合，支持 Windows 本地路径；已在 `GOOS=windows GOARCH=amd64` 下完成交叉编译检查。
- 强制终止后若存在未发布的临时文件，恢复扫描会忽略它；已发布块必须版本和内容匹配才会被使用。
