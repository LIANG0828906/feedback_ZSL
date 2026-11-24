# MorphEdge Studio

面向游戏与互动媒体素材的本地边缘检测 / 形态学处理桌面工作台。使用 Go 1.22、Wails v2 和纯 Go SQLite 驱动实现，包含确定性样本生成、分块阶段状态机、交互预览与精确重算、三路径一致性验证、检查点恢复和专业诊断界面。

## 已交付能力

- **确定性样本与真值**：固定种子生成单通道/三通道、含噪/低对比度 PNG；附带直线端点、圆掩膜中心与直径、边缘覆盖率、五个阶段的参考掩膜和 SHA-256 内容指纹。
- **加载校验**：PNG 损坏返回 `E_DECODE`；宽高或通道数不符返回 `E_META` 并给出 `width`、`height` 或 `channels` 字段；像素指纹不一致也会拒绝加载。
- **梯度与双阈值**：支持 Sobel、Prewitt、Central 梯度；8 邻域滞后双阈值。低阈值大于高阈值、阈值越界、算子未知返回 `E_PARA` 并定位字段。
- **形态学链**：`gradient → open → close → thin → prune` 五个独立阶段。开/闭运算、Zhang-Suen 细化迭代、短连通域剪枝均可配置；阶段输出严格保持尺寸与通道数。
- **预览/精确双路径**：参数拖动时按像素预算降采样、必要时简化结构元素，并记录倍率与简化策略；停止交互后锁定参数版本执行全精度重算。
- **自动一致性比对**：比较覆盖率偏差、几何偏差（端点、圆心、直径）和逐像素差异；超差返回 `E_DIVERGE`，附三项实测值、容差与首个不一致像素坐标。
- **分块状态机**：每个阶段/块具有 `pending → processing → committed` 与 `processing → failed → pending` 合法迁移；非法迁移返回 `E_STATE` 且原状态不变。
- **三条路径**：`realtime`、`resume`、`full` 使用相同参数快照与阶段顺序；测试逐像素、几何统计和提交序列三项一致。
- **SQLite 检查点**：每提交一个块阶段，在单个事务中串行写入快照与提交序号。加载时校验内容哈希、参数版本、算法版本、块几何与提交顺序；任一不匹配会删除该图像的全部快照后全量重算。
- **并发与异常隔离**：工作线程池限制最大并行度和每线程像素缓冲；排队等待原因写入 `poolStats.waitReasons`。单块故障只将该块阶段置为失败，其他块和已提交结果不受污染；界面支持重试。
- **诊断界面**：原图、预览、各阶段掩膜、差异热力图、真值几何表、块状态矩阵、运行摘要、事件日志，以及暂停/取消/导出。

## 目录

```text
cmd/verify/             本地全样本验收命令
internal/core/          样本、算法、状态机、线程池、检查点、三路径引擎
internal/workbench/     Wails 绑定服务、预览/精确会话、诊断视图与导出
frontend/dist/          无 Node 依赖的静态诊断界面
artifacts/verification/ 最近一次实际验收报告
scripts/                本机构建/运行辅助脚本
```

## 环境与构建

要求：

- Go 1.22+
- Wails CLI v2
- Windows：WebView2 Runtime；Linux：GTK 3 + WebKitGTK 4.1
- SQLite 使用 `modernc.org/sqlite`，数据库本身不需要 CGO；但 Wails/Linux 构建仍需要 CGO、GCC 与 `pkg-config`

标准 Ubuntu/Debian 环境：

```bash
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
go install github.com/wailsapp/wails/v2/cmd/wails@latest

go mod download
go test ./...
go test -race ./internal/...
go run ./cmd/verify
wails build -tags webkit2_41
```

当前验证环境没有 sudo，因此把缺失的构建依赖下载并解包到仓库内 `.sysroot/root`，使用非容器化本地 sysroot 完成了真实 `wails build`：

```bash
./scripts/wails-build-local.sh
./scripts/run-local.sh
```

`run-local.sh` 仅在系统缺少 WebKitGTK 工作进程路径时，从 `.sysroot` 准备 `/tmp` 下的等价本地运行前缀，不修改系统目录。

生产二进制位于 `build/bin/morph-edge-studio`。应用数据默认写入 `$XDG_CONFIG_HOME/MorphEdgeStudio`，也可通过 `EDGE_WORKBENCH_DATA` 指定本地可写目录。第一次启动会在 `<data>/samples` 生成全部默认样本，在 `<data>/checkpoints/workbench.db` 创建 SQLite 检查点。

## 算法约定

- 三通道图像按通道独立计算梯度、滞后阈值和后处理，输出通道数始终等于输入。
- 搜索阶段的工作只依赖像素值、参数快照、算法版本和规范提交顺序，不依赖线程 ID 或块到达顺序。
- 默认开/闭运算半径为 `0`（单位结构元素），避免对单像素宽的边缘直接做开运算而全部抹除。界面可独立调大半径；算法测试覆盖非零结构元素、细化和剪枝。
- 剪枝长度按 8 连通域的像素数解释。
- 预览默认目标约 42 万像素：大图降采样；开/闭结构元素按采样倍率放大以补偿降采样造成的边缘加粗，短线段剪枝阈值按降采样面积换算为等价物理长度，细化迭代保持原参数。预览记录倍率、缩放策略与简化标志，精确重算始终使用完整参数。
- 默认一致性容差：覆盖率 `0.05`、几何 `12px`、逐像素差异比例 `0.20`。容差随预览结果返回，验收报告记录实际偏差。

## 错误码

| 错误码 | 语义 |
|---|---|
| `E_DECODE` | PNG/清单无法解码 |
| `E_META` | 尺寸、通道、参考掩膜或内容指纹不一致 |
| `E_PARA` | 参数越界、阈值倒置、线程/缓冲边界非法 |
| `E_DIVERGE` | 预览与精确结果超差，或两路径像素/阶段不一致 |
| `E_STATE` | 非法块状态迁移、版本锁定失败等 |
| `E_CHECKPOINT` | SQLite 检查点读写/事务失败 |
| `E_RESOURCE` | 工作线程或像素缓冲资源错误 |
| `E_CANCELED` | 用户取消 |

## 实际验收

验证日期：2026-11-24。命令：

```bash
go test ./... -count=1
go run ./cmd/verify
```

全部核心/服务测试、`go vet` 与 Race Detector 通过。全样本验收结果见 `artifacts/verification/REPORT.md`，原始日志见同目录的 `test.log`、`race.log`、`vet.log`、`verify.log`、`wails-build.log` 和 `gui-smoke.log`。摘要：

| 样本 | 分辨率 | 通道 | 预览 | 预览/精确 | 三路径 | 检查点恢复 | 故障隔离 |
|---|---:|---:|---:|---|---|---|---|
| gray-640-clean | 640×480 | 1 | 15.3ms | PASS | PASS | 175 块 | PASS |
| rgb-960-noisy | 960×640 | 3 | 34.5ms | PASS | PASS | 350 块 | PASS |
| gray-1280-low-contrast | 1280×800 | 1 | 26.4ms | PASS | PASS | 630 块 | PASS |
| rgb-2048-wide | 2048×1152 | 3 | 74.3ms | PASS | PASS | 1320 块 | PASS |

所有样本预览均在 100ms 内返回；大图使用 3× 降采样，逐像素差异约 0.5%–0.9%，几何偏差不超过 9.2px。非默认开/闭半径、细化迭代与剪枝阈值的预览/精确容差另有自动化测试覆盖。三路径检查覆盖最终掩膜、几何统计和完整块阶段提交序列。检查点测试覆盖重开数据库、中途取消后的前缀恢复、内容哈希损坏、参数版本不匹配和算法版本不匹配；版本不一致会在加载时整体作废。故障注入测试覆盖块内 panic、其他块保持已提交和恢复后全量重算一致，工作线程池测试覆盖并行上限与排队原因记录。

Wails 生产构建已通过：

```text
Built '.../build/bin/morph-edge-studio'
```

应用在当前 WSL 桌面环境完成了受控启动（WebKit 网络/渲染子进程正常创建，持续运行至测试超时）；由于本机未在系统路径安装 WebKitGTK，启动检查使用了本地运行时前缀。标准桌面环境安装 `libwebkit2gtk-4.1-0` 后可直接启动。

## 复现说明

- 样本生成、算法、数据库和测试全部本地执行，不访问网络样本或账号。
- 固定 `SampleSpec` 会生成相同像素与指纹；验收命令可重复运行并覆盖报告。
- 检查点数据库路径可由 `EDGE_WORKBENCH_DATA` 隔离；测试使用临时目录，不污染应用数据。
- 界面导出会写入 `<data>/exports/<timestamp>`，包含原图、五个阶段 PNG 和 `diagnostics.json`。
