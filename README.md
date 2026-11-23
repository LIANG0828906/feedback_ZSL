# maskpipeline

面向游戏美术素材掩码分割的交互预览、精确重算、SQLite 分块检查点和独立验证工具。

项目只使用本地确定性样本，不访问外部账号或运行时网络服务。图像算法、真值、版本管理、检查点与验证逻辑均在仓库内实现，SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，不需要 CGO/MinGW。

## 环境

- Go 1.22 或更高版本（本工作区实测 `go1.22.12 linux/amd64`）
- 首次构建需要 Go module 代理下载 `modernc.org/sqlite v1.34.5`；`go.mod` 与 `go.sum` 已固定版本
- 运行阶段不访问网络，不使用 Docker 或其他容器
- CLI 默认路径全部为相对路径

## 快速开始

```bash
go test ./...

go run ./cmd/maskpipeline generate \
  -out samples \
  -seed 42

go run ./cmd/maskpipeline run \
  -samples samples \
  -db artifacts/checkpoints.db \
  -report artifacts/acceptance.json \
  -session-prefix acceptance

go run ./cmd/maskpipeline verify \
  -db artifacts/checkpoints.db \
  -sample samples/seed-42-384x256 \
  -session acceptance-incremental
```

`go run ./cmd/...` 会编译唯一的 CLI 入口；不带子命令时打印帮助。

## 交付内容

- `internal/sample`：多分辨率确定性样本、RGB 自描述图像、PNG 真值掩码、清单校验
- `internal/segment`：颜色/梯度阈值、确定性区域生长、开闭清理、区域统计与异常检测
- `internal/version`：不可变参数快照与单调版本号
- `internal/engine`：交互预览、精确重算、一致性比较、并行分块与阶段提交
- `internal/checkpoint`：SQLite 三阶段状态机、参数快照、内容哈希、规范提交槽位
- `internal/verify`：只依赖持久化阶段产物和真值指纹的独立重组验证
- `cmd/maskpipeline`：`generate` / `run` / `verify` 三个本地命令
- `samples/`：固定种子生成的三组样本
- `artifacts/acceptance.json`：当前实测验收报告
- `docs/DESIGN.md`：实现与一致性设计
- `docs/VALIDATION.md`：实际执行结果

## 样本格式

每个样本目录包含：

```text
sample_manifest.json
image.rgb
masks/region_01.png
masks/region_02.png
masks/region_03.png
masks/truth_labels.png
```

`image.rgb` 是 24 字节自描述头（magic、宽、高、通道数）加行优先 RGB8 数据。清单声明宽高、通道数、分块尺寸、预览延迟上限、真值指纹、每区域重心/面积/外接框/掩码路径与哈希，以及预览/精确一致性容差。

加载时：

- 清单缺失、JSON 损坏、图像数据损坏、真值掩码缺失/损坏：`E_DECODE`
- 图像头宽高/通道数与清单不一致、清单尺寸与真值掩码不一致：`E_META`，并设置具体 `field`

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_DECODE` | 样本、图像或掩码无法解码/缺失/内容损坏 |
| `E_META` | 尺寸、通道、哈希、统计或协议元数据不一致 |
| `E_PARA` | 参数越界或字段非法 |
| `E_SEED` | 种子位于背景、重复指向同区或清理后消失 |
| `E_SPILL` | 区域触边、面积异常或越出真值范围 |
| `E_DIVERGE` | 预览与精确结果的区域数/重心/面积超容差 |
| `E_VER` | 参数版本、算法版本混用或版本不存在 |
| `E_STATE` | 非法阶段状态迁移 |
| `E_BLOCK` | 单块阶段失败或持久化产物不一致 |
| `E_LATENCY` | 预览超过清单延迟上限 |

所有业务错误均为结构化错误，CLI 输出 `code`、`field`、`message`、`details`。

## 验证入口

```bash
go test ./...
go test -race ./internal/engine ./internal/checkpoint
go vet ./...
```

当前实测结果见 `docs/VALIDATION.md`，机器可读报告见 `artifacts/acceptance.json`。
