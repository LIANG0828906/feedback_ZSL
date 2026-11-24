# 技术说明与验证记录

## 1. 确定性样本模型

`internal/session/generate.go` 以固定种子（默认 `20261124`）生成完整会话，
同一种子逐位复现，不依赖外部网络或设备。

- 标称采样率所有通道相同（默认 48 kHz、2 s、4 通道），每通道写出一个
  `float64le` 裸文件，含前后各 `margin=64` 点边界裕量，保证插值不补零、不丢样。
- 时钟漂移以参数化真值给出：初相位 `phi0` + 分段线性速率节点
  （节点落在分段边界，速率单位为“每核心采样点”）。非参考通道速率取
  `[0.2,1.0] × MaxDriftPPM` 的单向非恒定值，累计漂移单调且足够大，避免与标记
  模板同量级造成检测歧义。
- 累计漂移 `phi(n)=phi0 + ∫₀ⁿ rate(t)dt`，生成端用前缀数组 `phiPrefix`
  O(1) 取址；估计端 `DriftParams.phiPrefix` 与其逐位同构。
- 标记为 Hann 加窗 sinc 脉冲或线性扫频（`pulse|chirp`），峰值归一化，按各通道
  分数到达位置 `margin + corePos + phi(corePos)` 以加窗 sinc 分数移位叠加，
  标记峰值精确落在真值到达点。
- 清单 `session.json` 携带解析真值（逐标记到达偏移、漂移曲线参数、残余上界），
  仅用于验收核对与诊断展示；处理流水线**不从真值反推采样**。

## 2. 装载与校验（要求 1）

`session.Load` 顺序执行：必填字段 → 编码 → 通道数 → 通道标识唯一 →
采样率一致 → 点数与清单一致 → 单参考通道。字段/点数问题返回 `E_META`
（带通道与字段名）；裸数据字节长度非 8 倍数、点数不符或出现 NaN/Inf 返回
`E_DECODE`。

## 3. 漂移估计（要求 2）

- 参考通道：全序列匹配滤波 + 非极大抑制 + 自适应阈值（全局最大峰 55%），
  再做三点抛物线亚采样细化。
- 非参考通道：围绕参考标记物理时间在物理漂移上界内开窄窗检测；缺标记或峰能量
  不足返回 `E_MARK`。
- 估计采用良态的两步最小二乘（`drift.go`）：
  1. 局部偏移 `d_m = arrival - margin - coreRef`；
  2. 相邻标记中心差分得到局部速率；
  3. 在分段边界节点上对速率做帐篷基加权最小二乘，得到分段线性速率曲线，
     `phi0` 由偏移减速率积分后的残差均值估计。
- 置信区间：基于残差标准差与设计矩阵逆的对角项，给 `phi0` 与每个速率节点的
  95% 半宽。标记数低于参数个数或设计矩阵退化返回 `E_DRIFT`（带标记数）。
- 真值核对只比较模型到达位置与真值，容差默认 0.25 采样点。

## 4. 分数延迟对齐与重采样（要求 3）

- 输出网格为参考核心时间轴，长度严格等于 `corePoints`。
- 对核心采样 `n`，本通道设备取址 `devPos = margin + n + phi(n)`，用预计算的
  加窗 sinc 核表（256 档小数相位量化）插值。量化是确定性的，因此任意线程数、
  任意处理路径都逐位一致。
- 边界用 `InBounds` 显式判断，越界即 `E_PARA`，绝不补零或丢样；`margin` 保证
  全程取址落在数组内。
- 对齐后在各标记窗内重新检测并与参考位置比较，残余超声明上界返回 `E_RESID`
  （带通道与标记序号）。插值阶数、单边窗长、容差、分段长度均可在配置与页面调整。

## 5. 分段状态机与提交边界（要求 4）

状态 `pending → aligned → committed`，失败分支 `→ failed`，`failed` 可回
`pending/aligned` 重试；`committed` 为终态，任何迁移都返回 `E_STATE` 且保持
原状态。已提交分段冻结漂移参数、对齐区间与内容哈希；`EditDrift` 触碰已提交
分段即拒绝，编辑只影响未提交分段。进度以“已提交/总分段”表示。

## 6. 并行调度与资源预算（要求 5）

`schedule.go` 使用固定工作顺序（先分段、后通道）的计划：

- 工作线程信号量限制最大并行度；全局重采样缓存令牌按
  `ResampleBudget / BufferCap` 限制缓存预算。
- 预算耗尽时任务在入口阻塞排队并记录 `budget_blocked` 事件，不丢弃任何分段。
- 计算可并行，但**提交严格按计划顺序**在单一提交循环完成，因此并行与串行产出
  相同提交序列；插值核表、漂移前缀与函数式工作单元保证逐样本一致。

## 7. SQLite 检查点与恢复（要求 6）

`store/checkpoint.go` 设单连接 + 单写入协程，所有写经串行通道进入，在一个事务
内原子写 `segments` 与 `commit_log`，写入不交错。每行持久化漂移参数 JSON、
对齐区间、float64le 输出 BLOB、内容哈希、参数版本、算法版本、表结构版本。

重启 `restore` 时先核对会话指纹、`param_version`、`algo_version`，再逐段核对
分段边界、输出可解码性与内容哈希；**任一不匹配即整库 Reset 全量重算**，禁止
部分混用旧分段。全部匹配时仅恢复已提交段，未命中段保持 pending 续跑。

## 8. 三路一致性与自动比对（要求 7）

`full`、`realtime`、`resume` 共用同一引擎核心（相同估计、核表、状态机与提交
顺序）。`CompareResults` 按 float64 位模式逐样本比对（不用容差），并核对漂移
参数；首个不一致即返回 `E_DIVERGE`，定位通道、分段、采样位置。提交序列与残余
统计提供独立比对器。实测三路逐样本一致、漂移参数与提交序列相同。

## 9. 诊断界面与异常链路（要求 8）

`internal/web` 通过 `go:embed` 内嵌单页应用，服务端强制回环绑定（非回环地址
直接拒绝）。页面展示漂移曲线、标记到达位置、对齐前后波形、残余偏移热力图与
分段状态表，支持调整插值阶数/容差/分段长度触发重算、逐段重试与导出。
`/api/inject` 可生成并立即验证通道缺失、标记缺失、增益溢出三类故障，返回带
定位的 `E_CHAN/E_MARK/E_CLIP`。单段异常只把该段置 failed，不污染其他段。

## 10. 实测验证结果（本机 Go 1.22.12，linux/amd64）

`go test ./... -count=1` 全绿，关键用例：

| 测试 | 验证点 | 结果 |
|------|--------|------|
| `TestGenerateAndLoad` / `TestLoadErrors` | 装载、E_META、E_DECODE | PASS |
| `TestFaultChannelMissing` | E_CHAN 定位 | PASS |
| `TestDriftEstimateAccuracyAndConvergence` | 速率误差 ≤1.5e-4、标记数增加误差下降、置信区间 | PASS |
| `TestDriftInsufficientMarkers` | 标记不足 E_DRIFT | PASS |
| `TestParameterValidation` | 阶数/窗长/速率越界 E_PARA | PASS |
| `TestStateMachine` | 非法迁移 E_STATE 且保持原状态、committed 冻结 | PASS |
| `TestThreePathDeterminism` | full/realtime/resume 逐样本、漂移、残差、提交序列一致 | PASS |
| `TestCheckpointResumeReuse` | 16 段全部恢复复用，输出与全量一致 | PASS |
| `TestSnapshotInvalidation` | 版本不匹配整库作废、无部分混用 | PASS |
| `TestBudgetBlockingRecorded` | 紧预算下出现 budget_blocked 且无丢段 | PASS |
| `TestFaultChains` | E_MARK、E_CLIP 链路定位 | PASS |
| `TestEditDriftFrozenRejection` | 编辑已提交段被 E_STATE 拒绝 | PASS |
| `TestCommitAndLoadOrder` | SQLite 串行原子、提交顺序稳定、Reset 清空 | PASS |
| `TestSinc* / TestDetectMarker / TestHasClip` | 插值、检测、削顶判定 | PASS |

CLI 端到端（pulse 与 chirp 各一次）：`generate → run → realtime → resume →
verify → export` 均成功，`verify` 输出
`VERIFY OK: full / realtime / resume 三路逐样本一致，提交序列一致`；
Web 注入三类故障分别返回 `E_MARK/E_CLIP/E_CHAN`，非回环绑定被拒绝。

性能：生成器经前缀化优化后，4 通道 ×96128 点会话生成约 34 ms；
三路一致性测试约 0.1–0.2 s。
