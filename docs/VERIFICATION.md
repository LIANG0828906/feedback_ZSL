# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）

环境：

- `go version go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.29.10`（纯 Go，无外部服务、无 Docker）
- 固定种子：`250250`

## 一键复现

```bash
./scripts/verify.sh
# 或
go test -count=1 ./...
go run ./cmd/acoustics verify --root .
go run ./cmd/acoustics serve --root . --addr 127.0.0.1:8088
```

浏览器打开 `http://127.0.0.1:8088/`。

## 自动化结果

`go test -count=1 ./...` 全部通过，完整逐条输出保存在 `artifacts/test-results.txt`。覆盖：

- 确定性样本结构校验；
- `E_SCHEMA` 字段缺失与对象标识；
- `E_GEOM` 房间重叠、墙段重合、门洞越界、法线非单位、坐标越界；
- 房间链、门洞链、定点长度和 `E_UNREACH` 搜索链；
- 镜像反射重复求解确定性；
- 状态机非法迁移返回 `E_STATE` 且原状态不变；
- 吸收系数/门洞删除的精确失效范围；
- `E_DIFF` 首个房间与字段定位；
- worker pool 缓冲耗尽排队事件；
- 单房间失败隔离与重试；
- SQLite 中断恢复、稳定提交顺序和签名失配整体作废；
- 增量预览、断点恢复、全量重算三路径一致；
- 本地服务状态/编辑 API。

竞态检测：

```bash
CGO_ENABLED=1 go test -race -count=1 ./pkg/acoustics ./pkg/engine ./pkg/pipeline ./pkg/server
```

结果全部 `ok`，输出保存在 `artifacts/race-results.txt`。

依赖已缓存在本机模块缓存；`GOPROXY=off go test -count=1 ./...` 同样通过，验证过程不需要访问线上站点或外部服务。

## 样本与真值

`go run ./cmd/acoustics verify --root .` 退出码为 0，完整 JSON 写入 `artifacts/acceptance-report.json`。

实际样本规模：

- 4 个矩形房间；
- 3 段共享墙、3 个门洞；
- 22 个反射面；
- 4 个声源；
- 6 个听者采样点；
- 24 个声源—听者对全部连通；
- 解析真值共记录 64 条有效一次反射路径，即每对 3 条有效反射。

真值校验结果：`checked_pairs=24`、`checked_rooms=6`、`ok=true`。

## 增量与恢复

验收编辑序列：

1. 将 `RF-R1-B` 吸收系数改为 `0.42`；
2. 拖动 `R3` 右侧边界从 `x=32` 到 `x=34`。

吸收系数编辑的失效清单精确包含 `RF-R1-B`、R1 的受影响 pair/listener；复用房间为：

```text
R2, R3, R4
```

最终边界编辑后，增量预览路径实际结果：

```text
reused_rooms     = R1, R2, R4
recomputed_rooms = R3
commit_sequence  = R1, R2, R3, R4
store order      = R1, R2, R3, R4
```

断点恢复路径在前两个房间提交后模拟中断，重开 SQLite 后：

```text
reused_rooms     = R1, R2
recomputed_rooms = R3, R4
store order      = R1, R2, R3, R4
```

一次性全量路径提交顺序同样为 `R1,R2,R3,R4`。`CompareReports` 对房间链、门洞链、定点长度、反射点、有效路径数、逐路径 SPL、聚合 SPL 和提交序列的比较通过；如遇不一致会返回首个房间与字段的 `E_DIFF`。

## 本地页面冒烟

实际 HTTP 冒烟输出保存在 `artifacts/http-smoke.txt`：

```text
GET /             status=200 bytes=15925
GET /api/state    ok=True progress committed=4/4 truth_ok=True
POST /api/edit    ok=True reused=['R1','R2','R4'] queue_events=5
```

队列事件来自诊断服务的 `max_workers=2, buffer_per_worker=1` 配置，证明缓冲耗尽时记录了排队原因而非静默丢弃。
