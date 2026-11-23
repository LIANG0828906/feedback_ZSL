# 实际验证记录

记录时间：2026-11-24（Asia/Shanghai）

## 环境

| 项目 | 实际值 |
|---|---|
| OS | Ubuntu 26.04 / Linux amd64 |
| Go | `go1.22.12` |
| `go env GOOS` | `linux` |
| Wails CLI | `v2.10.2` |
| 系统 SQLite | 未使用；采用 `modernc.org/sqlite v1.34.5` |
| Node/npm | 未用于前端构建 |
| Docker | 未使用 |

任务要求确认 `GOOS=windows`，但当前容器实际为 Linux。已使用 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 做交叉编译验证。

## 单元与集成测试

执行：

```bash
go test ./...
```

结果：

```text
ok tacticalworkbench/internal/sample
ok tacticalworkbench/pkg/tactical
ok tacticalworkbench/pkg/workbench
```

覆盖：

- 缺字段、成员重复、槽位有限性/越界、非法代价、不可通行标识、越界起终点。
- A* 重复搜索一致性、精确启发式、高估检测、溢出边界。
- 平滑拐点随容差收紧单调不减、代价不降低、双坐标读数。
- 队形越界 `E_FORM`、步长/转向 `E_PARA`、轨迹越线 `E_TRACE`。
- 区域封锁增量、缓存命中/重算、区间复用、增量与全量比较。
- SQLite 重启恢复、四类版本键任一失配整库作废、并发写入稳定顺序。
- 并行/串行窗口提交顺序和结果一致、状态缓冲耗尽阻塞。

## 样本生成

```bash
go run ./cmd/samplegen -out data/scenario.json
```

实际输出：

```text
wrote data/scenario.json (81378 bytes, seed=20261124)
```

同一种子重复生成 JSON 字节一致。

## 三路径验收

执行：

```bash
go run ./cmd/verify
```

实际结果摘要：

| 项目 | 结果 |
|---|---|
| 增量 vs 断点恢复 | `equal` |
| 增量 vs 全量 | `equal` |
| 恢复 vs 全量 | `equal` |
| 编辑 | 启用 `bridge-lock`，封锁 Alpha 后翼路径的格子 |
| 路径缓存 | `5` 命中，`1` 重算 |
| 推演区间 | `3` 复用（Bravo），Alpha 三个区间失效 |
| 失效清单 | `A-REAR` path + Alpha 三成员 simulation；含 tick 区间与原因 |
| 并行预算 | 最大 `4`，峰值 `4` |
| 状态缓冲预算 | 每 worker `4` |
| 提交顺序 | 并行与串行均为 `A-LEAD, A-RIGHT, A-REAR, B-LEAD, B-LEFT, B-REAR` |
| 提交互斥 | commit 回调峰值并发 `1` |

完整报告见 `artifacts/acceptance-report.json`。

## Wails 与 Windows

安装 CLI：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails version
```

实际版本：`v2.10.2`。

`@latest` 的 v2.17.1 要求 Go 1.26，而任务要求 Go 1.22；因此固定到兼容版本。

`wails doctor`：

- 成功扫描系统。
- 当前 Linux 容器缺少 `libwebkit2gtk`，因此不能启动 GUI。
- 环境依赖中有 GTK/WebKit/npm 的“可用但未安装”提示。

Windows 目标构建：

```bash
cd desktop
wails build -platform windows/amd64 -clean
```

实际输出：

```text
Built 'desktop/build/bin/tactical-workbench.exe' in 1.207s.
```

产物格式：

```text
PE32+ executable for MS Windows 6.01 (GUI), x86-64
```

另外验证：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o tactical-workbench.exe .
```

编译成功。

## 未覆盖项

- 因当前环境无 WebKit，未实际打开桌面窗口做人工点击验收。
- 未在真实 Windows 主机上运行 `wails doctor` 和启动 exe；仅完成 Windows 目标打包与 PE 格式检查。
- 未使用 Docker、外部样本、真实账号或远程服务。
