# 实际验证记录

验证时间：2026-11-27 05:02–05:10（Asia/Shanghai）

环境：WSL Ubuntu，`go version go1.22.12 linux/amd64`，`gcc` 可用。SQLite 驱动为 `github.com/mattn/go-sqlite3 v1.14.24`；Go 标准库提供 `net/http` 与 `database/sql`。

## 自动化测试

强制不用缓存：

```bash
gofmt -w cmd internal
go test -count=1 ./...
```

实际结果：

```text
?       lip-pipeline-prototype/cmd/lipctl       [no test files]
?       lip-pipeline-prototype/internal/cachegraph [no test files]
?       lip-pipeline-prototype/internal/domain   [no test files]
?       lip-pipeline-prototype/internal/generate [no test files]
?       lip-pipeline-prototype/internal/hashx    [no test files]
ok      lip-pipeline-prototype/internal/checkpoint 0.003s
ok      lip-pipeline-prototype/internal/convert    0.002s
ok      lip-pipeline-prototype/internal/pipeline   0.023s
ok      lip-pipeline-prototype/internal/schedule   0.144s
ok      lip-pipeline-prototype/internal/schema     0.023s
ok      lip-pipeline-prototype/internal/service    0.097s
ok      lip-pipeline-prototype/internal/web        0.030s
```

并发与数据竞争检查：

```bash
go test -race -count=1 ./internal/schedule ./internal/service ./internal/pipeline
```

实际结果：

```text
ok      lip-pipeline-prototype/internal/schedule 1.152s
ok      lip-pipeline-prototype/internal/service  1.506s
ok      lip-pipeline-prototype/internal/pipeline 1.161s
```

静态检查：

```bash
go vet ./...
```

退出码为 `0`。编译 mattn SQLite amalgamation 时会出现一条上游 C 代码的 `discards ‘const’ qualifier` warning，不影响构建和测试。

## 覆盖的验收点

- `internal/schema/roundtrip_test.go`
  - v1/v2/v3 均可加载；
  - 三版本回载后的包边界、音素顺序、逐帧口型和过渡位置与固定种子真值一致；
  - 未知版本和缺失 `pipeline.target_fps` 返回 `E_SCHEMA` 且错误路径含 `target_fps`；
  - v1 strict 导出产生 `E_LOSSY` 且仍可加载；
  - `x_conversion_shadow` 恢复精确分组和亲和度。
- `internal/convert/convert_test.go`
  - 采样点/毫秒、千分比/万分比整数往返；
  - 非对齐转换记录余数；
  - 溢出为 `E_OVF` 并带字段路径和两个操作数。
- `internal/pipeline/pipeline_test.go`
  - 包不超上限、不跨静音、顺序稳定；
  - 帧索引连续；
  - 每个分片预览与精确结果一致且在预算内；
  - 24 个并发构建结果完全相同；
  - 容差降为 1 时产生带帧号和实测跨度 2 的 `E_LIP`。
- `internal/schedule/scheduler_test.go`
  - 单 worker + 容量 1 时候选包缓冲耗尽会阻塞，并记录 `candidate_buffer` 等待时长；
  - 帧队列容量为 1 时背压阻塞且不丢帧；
  - 通道槽位为 1 时最大并发严格为 1；
  - 候选包超过帧预算返回 `E_VAL`。
- `internal/checkpoint/state_test.go`
  - `pending -> committed` 非法跳跃返回 `E_STATE`，原状态和进度不变；
  - 合法路径可到 `committed/progress=100`。
- `internal/service/acceptance_test.go`
  - 实时并行路径、一次性全量路径和断点恢复路径的规范化逐帧记录一致；
  - 新进程打开同一 SQLite：一个预提交分片被 recovered，其余五个 recomputed；
  - 破坏哈希/参数版本后该分片进入 invalidated 并重算，恢复后仍与全量一致；
  - 权重编辑同时存在失效节点和持久化复用命中；
  - 末音素边界编辑与全局 FPS 切换产生精确失效；
  - `E_ORDER/E_DECODE/E_VAL/E_STAGE` 均带对白、分片定位；
  - 单分片 `E_STAGE` 时提交数为 5/6，失败分片不提交，恢复补算后三路径一致；
  - 注入的 `E_DIVERGE` 带首个差异帧和包序号。
- `internal/web/server_test.go`
  - `/` 页面返回 200；
  - `/api/overview` 返回固定工程。

## 全新进程 HTTP 冒烟

启动：

```bash
go build -o /tmp/lipctl-verify ./cmd/lipctl
/tmp/lipctl-verify serve --addr 127.0.0.1:18092 --db /tmp/lip-verify-final-b72.db
```

实际响应摘要：

```text
overview: 6 shards 40 packs 444 frames
preview: True 44 / 81 within_budget= True
realtime: 444 records 6 commits
resume: 444 records recovered= 6 recomputed= 0
```

预览的 44 个采样点包含 stride=2 的降采样网格和所有音素/过渡边界锚点；这里的 resume 发生在 realtime 已提交全部六个分片之后；“只恢复一个预提交分片并补算五个”的断点场景由服务级测试用独立数据库验证。

异常接口实测：

```text
fault: order  http=422 E_ORDER  dialogue=dlg-101 shard_index=0 frame=11
fault: decode http=422 E_DECODE dialogue=dlg-101 shard_index=0 frame=None
fault: val    http=400 E_VAL    dialogue=dlg-101 shard_index=0 frame=None
fault: stage  http=422 E_STAGE  dialogue=dlg-101 shard_index=0 frame=0
```

`GET /api/schema-reports` 实测返回 v1/v2/v3 的 `load_report.fills`、换算记录和 `export_losses`；例如 v1 首条补齐路径为 `$.dialogues[0].shards[0].phonemes[0].categoryGroup`，默认值为 `upgraded-flat`。

## 手动验收建议

```bash
go run ./cmd/lipctl serve --addr 127.0.0.1:8080 --db artifacts/lip-pipeline.db
```

浏览器打开 `http://127.0.0.1:8080`：

1. 查看总览中的 6 个分片、40 个包、444 帧和三版本计数。
2. 选择分片，核对音素区间、包划分、帧条和 cache key。
3. 拖动“每包最大音素/过渡容差/采样步长”，查看预览采样、预算占用和一致标记。
4. 提交权重/边界编辑或切换 FPS，查看失效与持久化复用清单。
5. 点击实时下发、破坏哈希后恢复、断点恢复，观察 commit order 与 recovered/recomputed。
6. 点击异常按钮，查看错误码、JSON 路径、分片、帧号和操作数。
