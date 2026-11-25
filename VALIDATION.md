# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

环境：

- `go version go1.22.12 linux/amd64`
- `CGO_ENABLED=1`（本机测试环境）；交付构建已额外验证 `CGO_ENABLED=0`
- SQLite：`modernc.org/sqlite v1.29.10`（纯 Go）

## 命令与结果

```bash
GOTOOLCHAIN=local go fmt ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go test ./... -count=1
GOTOOLCHAIN=local go test -race ./internal/acceptance -count=1
GOTOOLCHAIN=local GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

结果：

- `go vet ./...`：PASS。
- `go test ./... -count=1`：PASS，原始输出见 `reports/go-test.txt`。
- `-race` 端到端验收：PASS，原始输出见 `reports/race-test.txt`。
- Windows 纯 Go 交叉编译：PASS，记录见 `reports/windows-build.txt`。

## 标准本地样本

运行：

```bash
OUT="$(mktemp -d /tmp/channelpipe-final-XXXXXX)"
go run ./cmd/channelpipe demo --workers 2 --out "$OUT"
go run ./cmd/verify \
  --sample "$OUT/sample" \
  --db "$OUT/checkpoint-main.sqlite" \
  --out "$OUT/standalone-verify.json"
```

最近一次临时输出目录由 `reports/final-temp-dir.txt` 记录；报告副本：

- `reports/validation-report.md`
- `reports/standalone-verify.json`
- `reports/demo-stdout.txt`
- `reports/verify-stdout.json`

样本规模：

- `color-320-rgba8`：320×240，8 位，RGBA。
- `gradient-1024-bgra16`：1024×512，16 位，BGRA。
- `alpha-1280-rgba16`：1280×720，16 位，RGBA。
- 真值采样：36 个点，每点 3 个条目，共 108 个阶段采样。
- 独立验证器扫描：4,568,064 个阶段像素。

## 验收点核对

| 验收项 | 实际结果 |
|---|---|
| 三版本加载 | v1 字段 `plan_ver`、v2 字段 `schemaVersion`、v3 字段 `version` 均加载并语义一致 |
| 未知版本 | 负向用例返回 `E_VERSION`，字段分别指向实际版本字段 |
| 缺字段路径 | `{"version":3,"plans":[{"id":"broken"}]}` 返回 `E_SCHEMA`，字段 `plans[0].width` |
| 升级痕迹 | 标准样本记录 42 条默认值补齐痕迹（v1/v2 各 21 条） |
| 降级损失 | v1/v2 各生成 `E_LOSSY` 元数据损失清单；核心转换语义往返一致 |
| 三路径一致 | 同一图像的 5 个修订版（原始、位深、填充/alpha、置换、顺序）像素和摘要一致 |
| 提交序列 | 三个路径每个修订版均为 `[0 1 2 3 4 5]`（128 分块，320×240 样本） |
| 断点续跑 | 中断路径在 2 个提交后取消，重开 SQLite 恢复遗留 `processing` 分块并完成 |
| 异常隔离 | 注入分块 1 首轮失败；首轮已提交 `[0 2 3 4 5]`，重试提交 `[1]` |
| 快照失配 | 自动化测试验证计划指纹变化后重开数据库会 reset，旧分块不可见 |
| 独立验证 | `cmd/verify` 直读 SQLite BLOB 与 truth，结果 `ok: true` |
| 资源边界 | 缓冲门探测真实阻塞 20ms 并记录等待；运行时排队事件写 `queue_events` |

## 说明

异常隔离演示中的最终原始提交日志为 `[0 2 3 4 5 1]`：这是因为分块 1 首次失败后，其他分块按稳定序号继续提交，随后显式重试分块 1。按计划指纹和修订版归一化后的三路径提交序列均为 `[0 1 2 3 4 5]`。
