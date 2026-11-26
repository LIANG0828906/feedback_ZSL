# 实际验证记录

验证时间：2026-11-27 04:45 (+08:00)

环境：

```text
go version go1.22.12 linux/amd64
Linux 6.18.58.1-microsoft-standard-WSL2 x86_64
```

原始输出保存在：

- `artifacts/verification/env.txt`
- `artifacts/verification/go-vet.txt`
- `artifacts/verification/go-test.txt`
- `artifacts/verification/go-test-race.txt`
- `artifacts/verification/http-e2e.txt`
- `artifacts/selfcheck.json`

## 1. 格式化、静态检查与单元/验收测试

执行：

```bash
gofmt -w ./... # 自有包；vendor 保持上游生成格式
go vet -mod=vendor ./...
go test -mod=vendor -count=1 ./...
CGO_ENABLED=1 go test -race -mod=vendor ./...
```

实际结果：

```text
go vet: 通过，无输出
go test: ok dnslab 0.023s；其余包无测试文件
go test -race: ok dnslab 1.175s；其余包无测试文件
```

重复确定性测试另外执行过：

```bash
go test -mod=vendor -count=3 ./...
```

结果通过。并发测试内部还以 worker `1/2/4/8` 各重复 5 轮，逐查询比较规则、候选依据、上游、跳数、来源、记录集合和剩余 TTL。

## 2. CLI 自检

执行：

```bash
go run -mod=vendor ./cmd/dns-workbench --selfcheck
```

实际汇总（最终一次）：

```text
ok=True
truth=39/39（3 个 schema × 13 条查询）
checks=14/14
wall_ms≈8.149
```

覆盖：

- V1/V2/V3 装载后规范工程 `reflect.DeepEqual` 等价；
- 每版 13/13 条真值一致；
- 增量结果与全量重算一致；
- SQLite 匹配版本保存/恢复；
- 9 类异常码：
  - `E_RULE_OVERLAP`
  - `E_UPSTREAM_REFERENCE_INVALID`
  - `E_TTL_OUT_OF_RANGE`
  - `E_TIMEOUT_OUT_OF_RANGE`
  - `E_BUDGET_OUT_OF_RANGE`
  - `E_HOPS_OUT_OF_RANGE`
  - `E_BACKPRESSURE_WORKER_BUDGET`
  - `E_CHECKPOINT_TRUNCATED`
  - `E_CHECKPOINT_VERSION_MISMATCH`

完整 JSON 见 `artifacts/selfcheck.json`。

## 3. HTTP 工作台端到端

实际启动纯本地服务并调用接口，结果保存于 `artifacts/verification/http-e2e.txt`：

```text
HTTP_E2E_OK
run_truth=13/13
run_wall_ms=0.215
edit_rebuilt=['rg-api']
edit_reused_count=8
replace_all_match=True schema_only=False
checkpoint_seq=13 segments=9 recover_restored=True
```

该轮验证执行了：

1. `POST /api/run`：4 worker 并行运行 13 条样本，全部匹配真值；
2. `POST /api/edit`：把 `R-002` 改到 U1，失效闭包精确重建 `rg-api`，其余 8 个运行期区域段复用，增量/全量语义一致；
3. `POST /api/replace`：整体替换回生成的 V3 快照，先识别 schema 再计算语义撤销，增量/全量一致；
4. `POST /api/checkpoint/save`：提交序号 13、9 个区域缓存段；
5. `POST /api/checkpoint/recover`：版本匹配并恢复；
6. 发送 SIGTERM，服务进入优雅关闭并保留最后完整检查点。

同内容跨 schema 的 `schema_only=true`、`propagation_closure=[]` 由 `TestIncrementalClosureAndEquality` 自动验证。

另一次独立 E2E 还验证了重启新进程后读取同一 SQLite 文件，恢复 `committed_seq=13`、9 个段，并通过 `/api/inject` 得到 `E_CHECKPOINT_VERSION_MISMATCH`。

## 4. 样本生成

执行：

```bash
go run -mod=vendor ./cmd/gen-samples -seed 42 -out samples
```

已生成：

- `samples/projects/v1-flat-literal-seconds.json`
- `samples/projects/v2-flat-list-millis.json`
- `samples/projects/v3-tree-absolute-negative.json`
- `samples/queries.json`
- `samples/truth.json`
- `samples/canonical-project.json`

## 5. 复核建议

```bash
make verify
make serve
# 浏览器打开 http://127.0.0.1:8080
```

在页面中可切换三版工程、点击查询行查看 actual/truth、施加编辑、查看传播闭包、保存/恢复检查点，并点击异常注入按钮查看稳定 `E_xxx` 码与上下文。
