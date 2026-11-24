# SIR 空间声学脉冲响应分析工作台

面向游戏与互动媒体音频素材的多通道脉冲响应（IR）分帧分析、声场参数重建、
脉冲响应渲染、误差传播报告与断点续跑工作台。全部样本由内置固定种子生成器
本地确定性产出，不依赖网络、真实声卡或外部音频资源。

## 环境准备

- Go 1.22+（实测 1.22.12），启用 CGO：`export CGO_ENABLED=1`
- C 编译器（gcc）；SQLite 驱动 `mattn/go-sqlite3` 自带 amalgamation，无需系统 `sqlite3.h`
- Node.js 18（已本地解压至 `tools/node-v18.20.4-linux-x64`）与 Wails CLI（已安装 v2.10.2）
- Linux 打包 Wails GUI 另需 webkit2gtk 开发头文件；缺失时使用内置 HTTP 面板

## 快速开始

```bash
go build -o bin/sir ./cmd/sir
./bin/sir generate --out data
./bin/sir verify --dir data --sample stereo_48k
./bin/sir run --dir data --sample stereo_48k --reset --mode realtime
./bin/sir run --dir data --sample stereo_48k --mode resume
./bin/sir serve --dir data --addr 127.0.0.1:8080
```

## 命令

| 命令 | 说明 |
| --- | --- |
| `generate` | 生成三组确定性样本（48k 立体声 / 44.1k 四声道 / 96k 5.1 六声道） |
| `run` | 按 `realtime/resume/replay` 模式运行分帧流水线，支持 `--pause`、`--reset` |
| `verify` | 三路径逐样本一致性、能量统计与误差上界验收 |
| `faultdemo` | 参数/输入注入 `chan/order/clip/frame/bound` 五类异常链路 |
| `serve` | 内置可视化面板（逐帧参数、延迟/能量曲线、误差上界、波形对比、暂停/重试/导出） |
| `export` | 导出报告 JSON |

## 错误码

`E_META` 元数据缺失 · `E_PARA` 参数越界 · `E_BOUND` 上界低于实测 ·
`E_STATE` 非法状态迁移 · `E_CHAN` 通道缺失 · `E_ORDER` 反射倒置 ·
`E_CLIP` 增益溢出 · `E_FRAME` 坏帧 · `E_DIVERGE` 多路径不一致。
所有错误均带帧号（如适用）、声道标识、字段名或指标名。

## 测试

```bash
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go test -race ./internal/pipeline ./internal/statemachine
go vet ./...
```

## 目录

- `internal/`：生成器、加载、分析、声场、渲染、误差、状态机、检查点、流水线、比对、面板
- `pkg/workbench/`：对外稳定 API（CLI / 面板 / Wails 共用）
- `desktop/`：Wails v2 桌面工程（独立 module）
- `docs/ARCHITECTURE.md`：架构与算法口径
- `docs/VALIDATION.md`：实际验证记录
