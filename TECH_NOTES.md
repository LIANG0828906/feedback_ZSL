# 房间混响区划工作台技术说明

## 运行

```powershell
go test ./...
go run ./cmd/workbench
```

默认地址为 `http://127.0.0.1:8080`，SQLite 检查点写入 `data/workbench.db`。可附加：

```bash
go run ./cmd/workbench -addr 127.0.0.1:18080 -db data/demo.db
```

驱动使用纯 Go 的 `modernc.org/sqlite v1.29.10`，兼容 Go 1.22，不需要 C 工具链。若本机 Go 环境曾启用一个不可用的 C 编译器，可执行一次 `go env -w CGO_ENABLED=0`。

## 目录

- `internal/wb/types.go`：领域模型、错误码、milli-unit 定点数。
- `internal/wb/sample.go`：固定种子 `26031125` 的四房间确定性样本。
- `internal/wb/validate.go`：`E_SCHEMA`、`E_GEOM`、`E_PARA` 校验。
- `internal/wb/geom.go` / `paths.go`：整型几何、墙段相交、房间图、最短房间链和折线穿墙检查。
- `internal/wb/audio.go`：定点衰减累加器、SPL 与解析真值。
- `internal/wb/engine.go` / `compute.go`：房间状态机、并发调度、原子缓存替换和提交。
- `internal/wb/edit.go`：边界拖动、参数修改、门洞增删、精确失效传播与续算。
- `internal/wb/compare.go`：增量结果与空缓存全量结果逐项比对。
- `internal/wb/checkpoint.go`：检查点载荷、内容哈希和版本校验。
- `internal/store/sqlite.go`：`database/sql` + modernc SQLite 的串行事务存储。
- `internal/web/`：`net/http` JSON API 与嵌入的原生 JS/SVG 诊断页。

## 关键规则

- 所有 dB、归一化密度和容差使用 `Fixed = int64` milli-unit；路径长度和坐标为整数。
- 路径选择按 `(门洞数最少, Manhattan 长度最短, 门洞链字典序)` 确定性排序。
- 路径折线在每个房间内尝试两种 L 形拐点，并用严格线段相交检查不会穿越非门洞墙段。
- 失效依赖显式记录为 `param:<房间>`、`geom:<房间>`、`door:<门洞>`、`conn:<房间>`、`listen:<听者>`、`commit:<房间>`。
- 门洞新增会检查新边可达连通分量；随后只对路径指纹或音频参数确实变化的听者重算。未变化对象输出 `reused-persisted-cache`。
- 每个房间一个 SQLite 事务；`SetMaxOpenConns(1)` 与存储层互斥锁保证写入串行、顺序稳定。
- 恢复时校验每个房间的 `input_hash`、`content_hash`、`param_version`、`algo_version`。任一失败会清空全部快照后全量重算，不混用新旧结果。
- 工作池默认 `maxParallel=2`，采样缓冲上限为每缓冲 16 个采样；缓冲耗尽会阻塞获取并记录 `sample-buffer-exhausted` 等待毫秒数，不丢弃采样。

## 主要 HTTP API

- `GET /api/state`：完整页面状态。
- `POST /api/boundary`：拖动共墙坐标，例如 `{"wallId":"W_V1","value":22}`。
- `POST /api/doors/add` / `POST /api/doors/delete`。
- `POST /api/rooms/params`：修改 RT60、早期反射密度或增益。
- `POST /api/recompute/full`、`POST /api/recompute/resume`。
- `POST /api/compare`：与临时空缓存全量计算逐项比对。
- `POST /api/truth/verify`：校验未编辑样本与解析真值。
- `POST /api/checkpoint/recover`：在不重启进程的情况下重放 SQLite 恢复。
- `POST /api/inject`：异常注入，类型为 `source_out_of_bounds`、`listener_out_of_bounds`、`door_nonadjacent`、`room_overlap`、`room_failure`。

JSON 错误体为 `{"ok":false,"error":{"code":"E_xxx","object":"...","field":"...","message":"..."}}`；同一 `E_xxx` 码也以 `ERROR code=E_xxx ...` 写入 stderr。

## 验证

完整测试输出保存在 `artifacts/go-test.txt`，真实 HTTP 验收摘要保存在 `artifacts/http-acceptance.txt`，执行记录见 `artifacts/verification.md`。
