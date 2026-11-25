# 实际验证记录

验证时间：2026-11-26 03:23 (+08:00)。

环境：

- `Linux ... microsoft-standard-WSL2 ... x86_64 GNU/Linux`
- `go version go1.22.12 linux/amd64`
- `GOOS=linux`、`GOARCH=amd64`
- 默认 `CGO_ENABLED=1`，但验收构建/测试额外显式使用 `CGO_ENABLED=0`
- SQLite：`modernc.org/sqlite v1.29.10`
- 模块缓存：`.../pkg/mod/modernc.org/sqlite@v1.29.10`，目录存在

## 命令与结论

```text
$ go mod verify
all modules verified

$ CGO_ENABLED=0 go build ./...
BUILD_OK

$ CGO_ENABLED=0 go test -count=1 ./...
ok  	volumefog	0.385s
?   	volumefog/cmd/workbench	[no test files]

$ go test -count=1 -cover ./...
ok  	volumefog	0.408s	coverage: 71.4% of statements

$ CGO_ENABLED=0 go build -o /tmp/volumefog-workbench ./cmd/workbench
/tmp/volumefog-workbench: ELF 64-bit LSB executable, x86-64, statically linked
	not a dynamic executable
```

完整 `go test -v .` 中 19 个测试全部 PASS，覆盖：

- 固定种子确定性生成与 schema 校验字段。
- `E_ORDER`、`E_DECODE`、`E_DENS`、`E_SEG`、`E_ITER`、`E_BLOCK`、`E_DIFF`、`E_DIVERGE`。
- 重叠段排序、定点量化残差和介质对定位。
- 分段透射率、累积散射、容差和单调收敛。
- 并行块上限、射线缓冲批次、阴影预算阻塞排队和等待时长。
- 单块失败、失败状态和重试不污染其他块。
- 体素/散射精确失效、缓存键不变/变化和 SQLite 持久化复用。
- SQLite 重启哈希/版本不匹配作废。
- 实时、断点恢复、全量重算的 12 块像素和提交序列一致。
- HTTP 诊断页、状态 API、注入和重试 API。

## 解析真值精度曲线

容差为分段透射率绝对误差 `0.012`、累积散射绝对误差 `0.025`。`normalized_deviation <= 1` 表示通过。

| 步进预算 | 最大透射率误差 | 最大散射误差 | 最大辐亮度误差 | 归一化偏差 |
|---:|---:|---:|---:|---:|
| 8  | 0.01995699 | 0.01689158 | 0.01586461 | 1.66308291 |
| 12 | 0.01364048 | 0.00927860 | 0.00903307 | 1.13670666 |
| 18 | 0.00952411 | 0.00698239 | 0.00681095 | 0.79367547 |
| 32 | 0.00605011 | 0.00396826 | 0.00389673 | 0.50417558 |
| 48 | 0.00204694 | 0.00191287 | 0.00158646 | 0.17057868 |
| 64 | 0.00174531 | 0.00157281 | 0.00154139 | 0.14544263 |

归一化偏差随预算增加单调不增。8 步预算下存在射线超过声明容差，`AssertRayTruthWithinTolerance` 返回带 `ray_id` 和 `deviation` 的 `E_ITER`；64 步全精度通过。

## CLI 自检

```text
$ go run ./cmd/workbench -verify
verification success=true report=artifacts/verification-report.json
precision normalized deviation=0.145443 monotonic=true
three-way equal=true scheduler_tiles=12 queue_records=24
injection E_SCHEMA passed=true actual=E_SCHEMA
injection E_SEG passed=true actual=E_SEG
injection E_ITER passed=true actual=E_ITER
injection E_DECODE passed=true actual=E_DECODE
injection E_ORDER passed=true actual=E_ORDER
injection E_DENS passed=true actual=E_DENS
injection E_BLOCK passed=true actual=E_BLOCK
```

机器可读报告已写入 `artifacts/verification-report.json`。三路径使用 32×18 视口和 8×6 分块，共 12 块；报告中 `three_way.equal=true`、序列长度 12，并包含 1 个被故意改坏后在恢复路径作废重算的检查点。

## HTTP 诊断台冒烟

实际启动：

```bash
go run ./cmd/workbench -addr 127.0.0.1:18088 -db artifacts/http-smoke.db
```

实测结果：

```text
页面包含：体积雾分块积分诊断工作台
/api/action?action=threeway -> threeway True 12 stale 1
inject kind=density         -> E_DENS fog-a {'x': 2, 'y': 2}
inject kind=block x=2 y=0    -> E_BLOCK {'x': 2, 'y': 0}
retry x=2 y=0                -> committed-retry
inject kind=block x=1 y=1    -> E_BLOCK {'x': 1, 'y': 1} medium=fog-b
```

诊断页可展示分块 T/S、平均颜色、缓存键前缀、阴影采样、射线缓冲峰值、资源占用、排队明细、失效块/复用块、依赖图计数、预览判定和三路径结论。
