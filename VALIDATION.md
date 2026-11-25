# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

## 环境

- 交付容器：Linux amd64，Go `go1.22.12 linux/amd64`。
- SQLite：`modernc.org/sqlite v1.34.5`（纯 Go，无 CGO 依赖）。
- Windows 兼容性：已执行 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`，结果通过。
- 已尝试通过 WSL 调用 `/mnt/c/Windows/System32/cmd.exe`：Windows shell 可启动，但当前 Windows 侧 `go` 不在 PATH/常见安装目录中，因此没有真实的 Windows `go env` 输出。代码已通过 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`。在安装 Go 1.22+ 的 Windows 上首次验收请运行：

```powershell
go version
go env GOPATH GOMODCACHE CGO_ENABLED GOOS
go run ./cmd/edgepipe serve --addr :8080
```

## 构建与测试

以下命令均实际执行通过：

```bash
go mod tidy
go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
go vet ./...
go test -count=1 ./...
go test -race ./internal/runner ./internal/store ./internal/state ./internal/diagnostic
```

`go test -count=1 ./...` 结果：

```text
ok  edgerefine/internal/diagnostic  0.064s
ok  edgerefine/internal/quantile    0.002s
ok  edgerefine/internal/runner      0.120s
ok  edgerefine/internal/sample      0.055s
ok  edgerefine/internal/state       0.002s
ok  edgerefine/internal/store       0.010s
ok  edgerefine/internal/verify      0.007s
ok  edgerefine/internal/web         0.051s
```

竞态检测的四个并发/状态/检查点相关包均通过。

## 确定性样本

`go run ./cmd/samplegen -dir samples` 实际输出：

```text
generated step128 128x96 edges=11 tile=32 seed=42026801 fp=09ac031ebd25
generated gradient160 160x128 edges=11 tile=32 seed=42026802 fp=c6681e79d380
generated spots96 96x96 edges=11 tile=32 seed=42026803 fp=936f2c665fdc
```

重复生成的 PNG 字节和真值指纹一致；损坏 PNG 的加载测试断言返回 `E_DECODE`，尺寸/预算越界测试断言返回带字段名的 `E_PARA`。

## 精度、预览与三路径

命令：

```bash
go run ./cmd/edgepipe three-path --sample step128
go run ./cmd/edgepipe three-path --sample gradient160
go run ./cmd/edgepipe three-path --sample spots96
```

实测汇总：

| 样本 | 三路径像素一致 | 精度报告一致 | 提交序列一致 | 中位偏差(px) | 最大偏差(px) | P95(px) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `step128` (`fa2b46617d3e1f2f`) | true | true | true | 0.2504 | 3.3187 | 0.4926 |
| `gradient160` (`3ab02d94c1aa4824`) | true | true | true | 0.2578 | 2.5760 | 0.5708 |
| `spots96` (`a11feedb023c65a1`) | true | true | true | 0.2611 | 3.4625 | 0.5912 |

三个样本均满足声明容差：中位误差 `<= 0.75px`，最大误差 `<= 4px`。

预览/精确发散检查：

| 样本 | 实际预览预算 | SE 步进 | 后端延迟 | 延迟上限 | 像素差异比例 | 发散判定 |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `step128` | 2 | 2 | 1ms | 100ms | 0.002848 | pass |
| `gradient160` | 2 | 2 | 2ms | 100ms | 0.001318 | pass |
| `spots96` | 2 | 2 | 2ms | 100ms | 0.004015 | pass |

默认像素差异容差为 `0.08`，因此预览近似路径与精确路径也在声明发散容差内。

与解析参考蒙版的二值 IoU 实测为：`step128=0.975`、`gradient160=0.978`、`spots96=0.960`；对应像素不一致比例分别为 `0.00659`、`0.00566`、`0.01053`。

`step128` 的结构元素半径扫描实测有符号中位偏差：

```text
r=0  -0.0199
r=1  -0.1238
r=2  -0.1261
r=3  -0.1551
r=4  -0.2344
```

序列非递增，方向单调可测；`/api/radius-sweep` 返回 `monotonic_non_increasing_signed_bias=true`。

## SQLite 恢复与独立验证

三路径命令会先写入 `块数 + 2` 个阶段，再关闭并重新打开 SQLite，模拟进程重启。恢复结果与一次性全量、交互精确路径的哈希和提交序列完全一致。

独立验证 CLI 实测：

```text
step128      ok=true rows=36 accuracy_pass=true median=0.2504 max=3.3187
gradient160  ok=true rows=60 accuracy_pass=true median=0.2578 max=2.5760
spots96      ok=true rows=27 accuracy_pass=true median=0.2611 max=3.4625
```

存储测试还覆盖：

- 内容哈希被篡改后 `LoadValidStages` 判定无效并清空该样本快照。
- 算法版本不匹配时整体作废。
- 12 个并发提交均成功，SQLite 写入由互斥锁和单连接串行化。

## HTTP 诊断页冒烟

实际启动最新二进制后验证：

```text
GET /                                  200 text/html
GET /api/samples                       200 application/json
GET /api/run?sample=step128            200，36/36 stages committed
GET /api/state?sample=step128          200，12 tiles × 3 stages matrix
GET /api/three-path?sample=step128     all_pixel_equal=true ...
GET /api/radius-sweep?sample=step128   monotonic=true
GET /viz?kind=original|separation|clean|skeleton|refined|overlay|heatmap  200 image/png
GET /api/export?sample=step128          200 application/zip，8234 bytes
```

未运行预览时请求 `/viz?kind=diff` 返回 `409 Conflict`，运行 `/api/preview` 后该图正常输出 PNG。
