# 交付说明与实测结果

本目录在空工作区中交付了一个纯 Go 领域组件、SQLite 检查点/缓存、三路一致性验证器、嵌入式本地 Web 诊断台，以及已初始化并接入同一领域模块的 Wails 桌面骨架。

## 快速运行

```bash
go test ./...
make verify
make schema
make ui
# 打开 http://127.0.0.1:8090
```

也可以直接运行：

```bash
go run ./cmd/prosodyctl verify \
  --workdir artifacts/checkpoints \
  --out artifacts/verification.json

go run ./cmd/diag \
  --addr 127.0.0.1:8090 \
  --workdir artifacts/diag
```

## 代码结构

- `internal/model`：内部工程、音素、帧包络、落点、拉伸结果模型与规范哈希。
- `internal/errs`：`E_SCHEMA`、`E_LOSSY`、`E_OVF`、`E_ENV`、`E_FIT`、`E_STATE` 结构化错误。
- `internal/generator`：固定种子 `424242` 的 4 条对白、PCM、音素、F0、权重和解析真值。
- `internal/schema`：`prosody.v1` / `v2` / `v3` DTO、装载、导出、默认值、损失报告和整数换算。
- `internal/sched`：最大并行度、每线程采样缓冲、待处理采样预算、阻塞排队指标和 panic 隔离。
- `internal/envelope`：纯整数分帧、平滑、归一化、定点域校验和响度/F0 双峰落点。
- `internal/fit`：静音保持、权重水填、上界触发、余数分配、串行区间和重音容差校验。
- `internal/dag`：组合缓存键与依赖图，传播 envelope → accent、stretch → range → warped accent。
- `internal/checkpoint`：纯 Go `modernc.org/sqlite`，单事务片段检查点和缓存，连续恢复边界。
- `internal/pipeline`：实时增量、崩溃恢复、全量重算三条路径，以及首个不一致定位。
- `internal/diag` + `internal/diag/web/index.html`：嵌入式无外网依赖诊断界面。
- `diagnostic`：供 Wails 等外部 Go module 集成的公开适配包。
- `wails-app`：Wails Vanilla 骨架；桌面壳启动同一诊断处理器并用 iframe 展示。

## 需求对应

1. **生成器与三版本装载**：生成器产出三版本 JSON 文档；`schema.Load` 自动识别版本，未知版本和必填字段缺失返回带 JSON 字段路径、源版本的 `E_SCHEMA`。
2. **升降级与损失报告**：低版本缺字段写入 `DEFAULT_FILLED`；v1 的厘赫兹和缺失上界生成 `E_LOSSY`，处置方式为原生字段降级/截断并写入 `x-` 无损扩展，导出文档可重新装载。
3. **基准与刻度换算**：采样/毫秒、千分比/万分比、厘赫兹/毫赫兹均为整数换算；余数进入 `Truncation`，溢出返回带字段和两个操作数的 `E_OVF`。生成样本的高 → v1/v2 → 高内容哈希完全相同。
4. **包络与落点**：帧长、帧移、平滑窗和定点域可配置；并行分块只影响计算顺序，帧结果按帧号归并。负包络或越界返回带帧号/音素号的 `E_ENV`。
5. **拉伸分配**：水填算法先满足静音最小保持，再按权重分配；触界段固定上界，剩余缺口继续水填，整数样本用最大余数法。每段记录上界、是否触发和依据。响度峰与基频峰距离超过容差时返回带音素编号和两侧实测采样点的 `E_FIT`。
6. **依赖图与精确失效**：缓存键为 `sha256(contentHash | phonemeID | paramVersion | kind | upstreamHash)`。目标时长、上界、容差或音素权重编辑后，清单逐项给出音素编号、失效原因、影响范围和复用来源；权重编辑时所有音素包络命中持久化缓存，未改变的系数/区间也会回查并校验旧缓存。
7. **SQLite 检查点与恢复**：每个 `(epoch, fragment)` 在一个事务中写入摘要、落点、拉伸 JSON、内容哈希、参数版本、算法版本和节点缓存。恢复时按连续逻辑序号扫描，任一内容/算法版本不匹配即判定整个快照边界无效并从最近完整提交之后继续；未提交片段没有退出报告。
8. **三路径与诊断 UI**：`pipeline.RunThreeWay` 使用相同工程和编辑序列运行实时增量、模拟 `SIGKILL` 边界后的 SQLite 重启恢复、忽略缓存的全量重算，并逐字段比较帧、落点、拉伸区间和提交序列。UI 展示 PCM 波形、包络曲线、重音线、前后音素区间、上界标记、失效/复用清单、调度预算指标、真值和三路径结果；可调整目标时长、上界和容差。

## 实测结果

### Go 测试

```text
go test ./...
ok  prosodyalign/internal/checkpoint
ok  prosodyalign/internal/dag
ok  prosodyalign/internal/envelope
ok  prosodyalign/internal/fit
ok  prosodyalign/internal/pipeline
ok  prosodyalign/internal/schema
```

### 三路一致性与真值

```text
$ go run ./cmd/prosodyctl verify ...
three_way_equal=true commit_sequences_equal=true epochs=5
truth dialog-a envelope_exact=true accent_error=5 within_tol=true fit_exact=true
truth dialog-b envelope_exact=true accent_error=5 within_tol=true fit_exact=true
truth dialog-c envelope_exact=true accent_error=5 within_tol=true fit_exact=true
truth dialog-d envelope_exact=true accent_error=5 within_tol=true fit_exact=true
```

完整 JSON 报告：`artifacts/verification.json`；运行日志：`artifacts/verify-run.log`。

默认编辑序列为：初始 → 目标时长 `+1200` → 默认上界 `2000` → 容差 `240` → 修改 `p5` 权重。权重编辑后的清单统计：

```text
phoneme_envelope reused=28 with_persistent_key=28
stretch_coefficient reused=20 with_persistent_key=20
stretched_range reused=8 with_persistent_key=8
invalid stretch_coefficient=8, stretched_range=20, warped_accent=4
```

这表示 4 条对白中各有 2 个耦合分配段实际改变，其余系数复用；串行区间只影响从变更边界开始的下游。

### Schema 往返

`go run ./cmd/prosodyctl schema-check` 显示 v1/v2/v3 装载后的 `content_hash` 均为：

```text
599258b36d51837899787d70af70da14275207b80b8b94b44b6a63ac3fea7b1f
```

报告：`artifacts/schema-check.json`。

### 本地诊断界面

```text
$ go run ./cmd/diag --addr 127.0.0.1:18091 --workdir artifacts/diag-final
prosody diagnostic UI: http://127.0.0.1:18091

GET /api/health -> {"ok": true, "service": "prosody-diag"}
INDEX_BYTES=14661
edit_ok= True epoch=1 invalid=40 reused=52 queue_wait_count=12
three_way_equal= True commit_sequences_equal= True
recovery_completed= 0/dialog-a 0/dialog-b 0/dialog-c 0/dialog-d 1/dialog-a
```

冒烟记录：`artifacts/ui-smoke.md`。故意把容差改为 `0` 时，4 条对白均返回 `E_FIT`；恢复到 `480` 后全部成功，验证异常隔离和可重试。

### Wails

- `wails init -n prosodydesktop -t vanilla -d wails-app` 已完成。
- `cd wails-app/frontend && npm install && npm run build` 通过。
- Wails module 已通过 `replace prosodyalign => ..` 接入公开 `diagnostic` 包。
- 当前容器中 `wails build` 的实际阻塞原因是系统缺少 `gtk+-3.0` / `webkit2gtk-4.0` 开发包；`wails doctor` 也给出相同结论。在带桌面依赖的 Ubuntu/WSL 中安装对应包后可执行 `make wails-build`。若发行版只提供 WebKit 4.1，可尝试 Wails 对应 build tag（例如 `-tags webkit2_41`）。

## 设计取舍

- 包络使用绝对振幅和整数均值/平滑/归一化，避免浮点调度顺序影响；PCM 仍由正弦波确定性合成以供波形展示。
- 真值由生成器在构造阶段独立求和/水填规则生成，不读取 pipeline 输出；测试逐字段比较。
- 拟合求解本身是全局廉价规划，因此允许先求解再比较节点载荷；“失效”和“复用”以实际采样/系数是否变化及持久化键命中为准，而不是只要参数版本变化就清空全部结果。
- 调度器记录亚毫秒等待的纳秒值；预算不足时阻塞入队并施加反压，不丢弃任务。
