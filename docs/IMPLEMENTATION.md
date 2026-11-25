# VolFog 实现说明

VolFog 是一个仅依赖 Go 标准库与 `modernc.org/sqlite`（纯 Go SQLite 驱动）的确定性体积雾分块积分内核。模块名为 `volfog`，Go 基线为 `1.22`。

## 快速运行

```bash
go version
go build ./...
go test ./... -count=1
go run ./cmd/fogdiag --verify
go run ./cmd/fogdiag -addr 127.0.0.1:8080 -db artifacts/volfog-diag.sqlite
```

浏览器打开 `http://127.0.0.1:8080/`。界面与静态资源由 `diag/web/index.html` 内嵌，不访问外网、CDN、账号或容器服务。

## 目录与职责

- `fog/scene.go`：确定性种子生成器、介质、整型体素密度场、相机轨迹、配方和固定样本。
- `fog/validate.go`：`E_SCHEMA` / `E_ORDER` / `E_DENS` 校验，错误带介质 ID 与字段路径。
- `fog/geom.go`：Vec3/AABB、Slab 区间求交、透视相机与像素中心射线。
- `fog/density.go`：整型体素定位、Amanatides-Woo 体素遍历、密度线积分。
- `fog/tile.go`：固定屏幕块、像素归属和块边界。
- `fog/integrate.go`：定点区间、重叠段分段、HG/各向同性相位、确定性阴影采样、步进积分与解析分段真值求解。
- `fog/render.go`：并行块调度、块槽、每线程射线缓冲分块、阴影预算闸门、预览/全精度帧和 `E_DIVERGE` 比较。
- `fog/deps.go`：介质密度/系数、相机、配方、射线段、块、像素依赖图；体素读集缓存键与精确失效传播。
- `fog/store_sqlite.go`：SQLite 表结构、事务原子提交、串行写入、提交序列、块缓存和重启哈希/版本校验。
- `fog/pipeline.go`：增量、恢复、全量三条检查点路径与强制块重试。
- `fog/verify.go`：真值核对、三路径报告和首个块/字段差异定位。
- `fog/codec.go`：带 CRC32 的介质二进制编解码及 `E_DECODE` 注入边界。
- `diag/service.go`、`diag/web/index.html`：本地 JSON API 与诊断工作台。
- `cmd/fogdiag/main.go`：工作台服务和 `--verify` 命令行报告。

## 关键设计

### 确定性与解析真值

生成器固定种子为 `0x564f472d32303236`，产出 `48×27` 视口、`8×8` 块（共 24 块、1296 像素）、3 个 AABB 介质、3 个逻辑时刻相机采样，以及 18 步全精度 / 6 步预览、4 阴影采样 / 1 预览采样的配方。

真值不是外部数据文件。生成器调用分段常数体素的精确齐次单元公式：在所有体素边界处切分，每段使用

- `T *= exp(-σ_t dt)`
- `L += T_before · (σ_source / σ_t) · (1 - exp(-σ_t dt))`

因此它直接来自生成器的几何、密度、系数、相位和光照构造。当前样本记录 8 个中心稳定像素真值与 9 条密度积分真值；数值全精度路径与像素真值的最大透射率误差约 `7.77e-16`，最大辐亮度误差约 `1.04e-17`。

### 区间、重叠与定点

`TraceRay` 对每个 AABB 求原始参数区间，用配方的 `1e6` 定点刻度量化端点并记录量化残差。所有介质端点与射线 near/far 端点合并、排序、去重，再形成 partition：

- 无活动介质：显式 `empty` 段；
- 一个或多个介质：`medium` 段；
- 重叠区域不把长度简单相加，而是保留所有 `MediumIDs`，按 `split-at-all-endpoints;composite-sum-within-overlap` 规则求复合系数。

追踪结果包含稳定 `Digest`，重复追踪和并发调度不改变分段。

### 预算调度

`BudgetGate` 用容量为 `MaxParallelBlocks` 的 channel 限制并行块；射线按 `RayBufferPerWorker` 分块生成和追踪，当前样本为每线程最多 32 条；阴影采样先由 `Integrator.ShadowCost` 精确计数，再从总预算保留。

块槽或阴影预算不能立即满足时写入 `QueueEvent{BlockID, Reason, WaitMS}`，不会丢块。阴影额度可由下一帧或调度补充显式增加。正常样本预算充足，并行块槽仍会产生可观察的 `max_parallel_blocks` 排队记录。

### 预览 / 全精度

预览路径使用配方中的较少步进和阴影样本，记录实际 `StepBudgetPerSpan`、`ShadowSamplesPerStep`、总步进/阴影数、耗时、延迟上限和判定依据。两条路径从同一个 `Scenario` 指针生成帧，因此共享介质快照版本、相机版本和配方版本。

`ComparePreviewAndFull` 先比较版本，再按 `(y,x)` 扫描所有像素；首个超容差像素返回 `E_DIVERGE`，明细包含坐标、两条路径的 T/L 实测值、容差和版本集合。

### 精确依赖与缓存键

依赖图显式建立：

- `medium_density` / `medium_coefficients`
- `camera` / `recipe`
- `ray_segment`
- `tile` / `pixel`

并通过 `VoxelRefs[voxelKey] -> []RayRef` 支持体素级反查。编辑声明的 `fog-pattern-right[2,3,4]` 体素时，只有块 `[9,10]` 失效；其余 22 块命中持久化缓存。

缓存键没有粗糙地只使用整个介质根哈希，而是包含：

- 射线/分段摘要与定点区间；
- 该段实际读取体素的值哈希集合；
- 吸收/散射/各向异性系数哈希；
- 相机版本、配方版本、算法版本；
- 上游分段缓存键。

完整介质 `ContentHash` 仍写入 `medium_hashes_json` 供审计和重启校验。这样既满足“介质内容哈希 + 射线 ID + 参数版本 + 上游键”的键公式，又不会因未被某块读取的体素变化而误失效。密度写入值未变化时，应用层返回 `changed=false` 和空失效清单。

### SQLite 检查点与三路径

数据库包含：

- `revisions`：一次目标参数快照的渲染修订；
- `block_checkpoints`：块 ID、顺序号、射线摘要、T/L 数组、完整结果 JSON、缓存键、体素/介质哈希、内容哈希、相机/配方/算法版本；
- `commit_events`：自增串行提交事件；
- `block_cache`：按缓存键复用的不可变块结果。

`CommitBlock` 在单个 SQLite 事务中写 checkpoint、commit event 和 cache row；`CheckpointStore.mu` 与 `SetMaxOpenConns(1)` 保证写入不交错。规范提交顺序为块 ID `0..23`。

三条验收路径为：

1. 一次性全量重算到新 SQLite 文件；
2. 先提交原始场景，再编辑密度并增量渲染，仅重算 `[9,10]`，其余 22 块缓存命中；
3. 在第 7 个块事务后返回 `ErrSimulatedCrash`，关闭并重开 SQLite，只恢复已提交的 `0..6`，剩余块续算。

三者像素逐值相同、失效清单相同、目标修订的提交序列均为 `0..23`。`CompareBlockResults` 会定位首个不同的块号与字段，例如 `block[9].radiance[12]`，并换算像素坐标。

重启校验会检查相机、配方、算法版本以及每行的快照内容哈希和介质哈希；任一不匹配则把修订标记为 `invalid_*`，整个修订不能作为恢复依据。不可变 `block_cache` 仍可让未受影响且缓存键相同的块被新修订复用。

### 异常注入

- `E_DECODE`：编码介质后翻转一个负载字节，CRC32 校验失败并保留介质 ID。
- `E_ORDER`：把第二个相机时刻改到第一个之前，返回 `cameras[1].time`；它同时可 `errors.Is(err, ErrSchema)`。
- `E_DENS`：编辑越界体素，错误包含介质 ID 和 `density.voxel[...]` 字段。
- `E_BLOCK`：故障控制器让指定块失败；其他块继续并提交，失败块不写 checkpoint。清除故障后可强制重算并提交该块。

SQLite 事务和 `synchronous=FULL` 保证强杀进程后只有已提交事务可恢复；系统不承诺生成退出报告。测试中的 `CrashAfterCommits` 是在事务成功返回后的安全点模拟中断。

## HTTP API

- `GET /`：诊断工作台。
- `GET /api/state`、`GET /api/graph`。
- `POST /api/render/preview-compare`。
- `POST /api/render/checkpoint`。
- `POST /api/edit/density`、`POST /api/edit/coefficient`。
- `POST /api/faults/inject`、`POST /api/faults/retry`。
- `POST /api/verify/truth`、`POST /api/verify/paths`。

工作台展示 T/L 热力图、块边界、预算占用、峰值并行、阴影用量、排队明细、失效块、`medium -> voxel/field -> segment -> block -> pixels` 传播链以及结构化错误。
