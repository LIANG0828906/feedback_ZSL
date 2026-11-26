# MaskQA 笔画方向与连通拓扑核验工作台

本仓库在空目录上独立初始化，Go module 为 `maskqa`，只使用 Go 标准库。根包 `maskqa` 是可集成领域组件，`cmd/maskqa` 提供本地 CLI 与 HTTP 诊断应用。

## 启动与验证

```bash
go version                 # 实测 go1.22.12 linux/amd64，满足 >=1.21
go test ./...
go vet ./...
go run ./cmd/maskqa -verify -checkpoints .maskqa-checkpoints
go run ./cmd/maskqa -gen-dir samples
go run ./cmd/maskqa -addr 127.0.0.1:8080 -checkpoints .maskqa-checkpoints
```

浏览器打开 `http://127.0.0.1:8080`。页面无外部 CDN、字体或网络依赖。

HTTP 接口：

- `GET /api/state?sample=0|1|2`：工程、真值、积算掩码字节、分块、预算、依赖图和报告。
- `POST /api/edit`：编辑起止坐标、权重或工序优先级并执行增量失效。
- `POST /api/reset?sample=0`：关闭会话并清理该样本检查点后重建。
- `GET /api/export?sample=0&schema=v1|v2|v3`：下载版本化 JSON 工程。
- `GET /api/roundtrip?sample=0&schema=v1|v2|v3`：执行导出、对应版本加载并返回痕迹/降级清单。

## 已实现能力

### 确定性样本与三版本装载

- 固定种子 `GeneratorSeed = 0x4D41534B5141`，生成 `96×64`、`128×80`、`160×96` 三张不同分辨率样本。
- 图标包含近色渐变背景、前景色块、半透明笔画和嵌入式确定性噪声。
- 候选掩码包含边界内缩、受控空洞和贴边外溢。
- 施工记录包含笔序号、工序优先级、分组、起止像素、宽度、硬度和覆盖权重。
- 真值由生成器构造并经同一拓扑统计算出，包含重心、面积、外接框、孔洞、环带宽度、方向类别、像素参考掩码和关联笔画。
- V1 使用 `proj_id`、`stroke_no`、`x0/y0/...`、`cover_weight_permille`，无硬度/方向字段。
- V2 使用 `projectId`、`sequence`、`startOffsetX/...`、`weightPermyriad`、`hardnessPermyriad` 和数字 `directionCode`。
- V3 使用 `id`、`seq`、绝对 `start/end`、`weight_permyriad`、`hardness_permyriad` 和字符串方向类别。
- 未知 schema 返回 `E_DECODE`，未知版本会带 schema 字段路径；必填字段缺失返回带 JSON 路径的 `E_DECODE`。

### 转换、降级与整数往返

- 低版本缺失硬度时补 `6500/10000`，缺失方向时补 `unknown`；每条痕迹包含字段路径、`<missing>` 旧值、默认值和规则。
- V1 导出会为分组、方向和非默认硬度生成 `E_LOSSY` 清单及处置说明。
- V1 标准字段保持缺失，同时写入 `x_maskqa_recovery` 审计扩展，使本工作台导出的产物可还原等价分组/硬度/方向；普通无扩展 V1 文档仍严格走默认补齐规则。
- 千分比乘 10 转万分比；万分比除 10 时记录商、余数、取舍方向和字段位置。V1 可选 `weight_remainder_bp` 保存余数，因此高→低→高的权重完全不变。
- V2 的序号原点由 `SequenceOrigin(seq, axis, dimension)` 纯整数推导，偏移和绝对坐标通过带溢出检查的加减往返。
- `E_OVF` 错误包含字段和两个操作数；`E_PARA` 包含字段名及 `[min,max]` 合法区间。

### 栅格、合成、拓扑与方向

- 单笔使用整数点到线段距离平方、硬度内圆和线性平方衰减生成覆盖分量；同参数输出逐像素确定。
- 覆盖权重按 `weight_permyriad * 255 / 10000` 得到最大 alpha，再使用确定性 8-bit rounded source-over 与候选掩码合成。
- 合成次序唯一为“工序优先级升序，同优先级按笔序号升序”；每个分块记录参与笔画和裁决原因。
- 拓扑使用 8 邻接连通域、4 邻接背景孔洞洪水、整数重心和 chamfer 距离环宽。
- 方向核验使用 Sobel 边界法向、法向旋转切向投票、笔画向量点积对齐评分；环形域结合孔洞和法向分散度判为 `closed_ring`。
- 孔洞、环宽或其他统计超差时返回 `E_TOP`，错误含域号、字段路径、期望值和实测值。

### 依赖图、增量失效与调度预算

- 图中显式包含 `stroke:*`、`order:composition`、`tile:x,y`、`verify:topology`、`verify:direction` 节点和依赖边。
- 坐标编辑失效旧/新 `StrokeBounds` 的并集覆盖分块；权重编辑失效该笔 bounds 内分块；优先级编辑失效该笔参与的分块。所有变更都会使拓扑和方向核验节点失效。
- 缓存键由分块输入哈希、笔画 ID、笔画参数哈希、分块内次序、`OrderVersion`、`ConversionVersion` 和上游键共同哈希得到。
- 未失效分块从持久化的最后笔画记录恢复字节并标记 `reused`，不重新栅格化。
- 调度器限制 worker 数、每缓冲像素上限和待处理队列容量；队列/缓冲耗尽时阻塞并记录原因、次数和等待毫秒。单个 job panic 被隔离为 `E_WORKER_PANIC`，不影响其他分块。

### 原子检查点与三路径一致

- 检查点目录：`.maskqa-checkpoints/.../commits/000000000001.json`。
- 每个笔画-分块提交记录分量、提交后分块、笔画/分块缓存键、内容哈希、输入哈希、换算残差摘要、次序/换算/算法版本。
- 写入采用同目录临时文件、`fsync`、原子 `rename`、目录 fsync、进程内 mutex 和 `flock`，保证串行且不交错。
- 重启时顺序校验所有提交；任一内容哈希或版本不匹配会报告 `E_CKPT_VERSION_HASH`/`E_CKPT_SEQUENCE` 并整体清空重建。残留 `.commit-*.tmp` 视为强制终止撕裂写，忽略并只承认已 rename 的最后连续提交。
- 测试 `TestCheckpointForceKillSubprocess` 实际启动测试二进制辅助进程，提交 3 个笔画记录后被 `SIGKILL`；父进程重开存储并逐像素哈希核对恢复结果。
- `TestThreePathsCheckpointRecoveryAndDiff` 对同一工程和同一编辑序列比较实时增量、SIGKILL/重开恢复和一次性全量重算；`CompareResults` 比较像素、提交序列、连通域统计和方向类别。
- 不一致时返回 `E_DIFF`，`Error.Location` 给出首个分块坐标、像素坐标和字段名。

## 本地界面

页面右侧可选择笔画并修改起点、终点、权重和优先级。主画布显示：

- 红色：候选掩码；
- 青色：积算掩码；
- 绿色：参考真值；
- 叠加亮色：候选与积算重叠。

其他面板展示逐域面积、孔洞、环宽、方向评分/类别、偏差、失效分块与复用分块、分块缓存键/裁决依据、worker/队列/缓冲预算和依赖传播链。

## 实测结果

以下结果在本次交付环境实测：

- `go version`：`go1.22.12 linux/amd64`。
- `go vet ./...`：通过，无输出。
- `go test ./...`：通过；13 个显式测试全部 PASS，辅助 kill target 按预期 SKIP。
- `go test -race ./...`：通过。
- `go run ./cmd/maskqa -verify ...`：三张样本分别输出 24/40/60 个分块，均为 3 个连通域且 OK。
- `go run ./cmd/maskqa -gen-dir samples`：已生成三张图标的 V1/V2/V3 JSON、图标 PNG、候选/参考/积算掩码 PNG。

主要源码：

- `model.go`、`errors.go`：统一模型和业务错误。
- `generate.go`：固定种子生成器。
- `schema_dto.go`、`schema_load.go`、`schema_export.go`、`convert.go`：三版本 schema、整型换算和痕迹。
- `raster.go`、`engine.go`、`scheduler.go`：栅格、合成、缓存和预算调度。
- `topology.go`、`direction.go`、`verify.go`、`diff.go`：拓扑、方向、核验和 E_DIFF。
- `dependency.go`、`workbench.go`、`checkpoint.go`：失效图、增量工作台和持久化恢复。
- `server.go`、`web/index.html`、`cmd/maskqa/main.go`：HTTP/CLI 与交互页面。
