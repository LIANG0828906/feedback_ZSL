# 实际验证结果

验证时间环境：Linux 容器，`go version go1.22.12 linux/amd64`。同一代码已用 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64` 成功交叉编译为 PE32+ `roomreverb.exe`，因此不依赖 C 工具链。

原始输出保存在：

- `artifacts/verification-commands.txt`：`go version`、`go vet`、`go test ./...`、Windows 交叉编译。
- `artifacts/verification-report.json`：`roomreverb verify` 的结构化总验收报告。
- `artifacts/http-smoke.txt`：真实 `net/http` 接口、增量编辑、比对、`E_SCOPE` 和重启恢复冒烟。

## 自动化测试

执行：

```bash
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./... -count=1 -timeout 60s
./bin/roomreverb verify
```

结果：全部包通过，`go vet` 退出码 0。覆盖：

- 9 个声源—听者对的房间链与真值逐项一致，折线均不穿墙。
- `E_SCHEMA`、`E_GEOM`、`E_UNREACH`、`E_PREC`、`E_STATE`、`E_SCOPE`、`E_DIFF` 和 SQLite 哈希/版本失效。
- 增删门洞、移动墙线、不可达后恢复、非法状态迁移。
- 全量、增量、SQLite 恢复三条结果路径逐项相等。

## 三条 S1/S2/S3 → L1 路径

固定样本中的三条重点路径为：

- `S1->L1`：`R1 → R2 → R5`，门 `D12,D25`。
- `S2->L1`：`R3 → R2 → R5`，门 `D23,D25`。
- `S3->L1`：`R6 → R5`，门 `D56`。

结构化报告中的 `three_listener_l1_paths` 为 `["S1->L1","S2->L1","S3->L1"]`，三条链、长度、曲线和声压级均通过真值容差。

## 精度结果

最大绝对误差（来自 `artifacts/verification-report.json`）：

| 精度档 | 小数位 | 曲线最大误差(dB) | SPL 最大误差(dB) |
|---|---:|---:|---:|
| coarse | 3 | 6.0763719e-02 | 2.5258200e-02 |
| standard | 6 | 3.9553773e-05 | 2.9701864e-05 |
| tight | 9 | 5.2670536e-08 | 5.3390089e-08 |

曲线和 SPL 的最大误差均随精度档收紧单调不增。标准档声明容差为曲线 `0.004 dB`、SPL `0.008 dB`；实测远小于容差。路径长度最大误差约 `2.75e-6` 坐标单位，标准档长度容差为 `0.05`。

## 三计算路径一致性

`roomreverb verify` 会：

1. 从空缓存执行确定性全量计算并核对解析真值。
2. 写入 SQLite 基线，编辑 `R2` 的 RT60，执行精确失效和增量重算。
3. 在编辑后的同一关卡上从空缓存再做全量计算，与增量合并结果逐项比较。
4. 关闭 SQLite，重新从编辑后的 JSON 和数据库启动，校验哈希/版本并恢复，再与新鲜全量结果逐项比较。

本次记录中：

```text
incremental_compare_ok = true
recovery_compare_ok    = true
recovered_rooms        = 6
scope_error_code       = E_SCOPE
scope_first_room       = R1
scheduler_wait_recorded = true
```

调度缓冲验收在保存的这次运行中记录到 `76 ms` 等待；等待时长会随调度抖动变化，但事件原因固定为 `per_worker_sample_buffer_full`，且任务不丢弃。

## HTTP 冒烟

`artifacts/http-smoke.txt` 记录：

```text
progress= 6/6 recovered= 0 paths= 9
edit R2: invalid_rooms=['R2'], invalid_listeners=['L2']
         reused_rooms=['R1','R3','R4','R5','R6']
compare: ok=True
scope: E_SCOPE R1
restart: recovered all 6 rooms, progress=6/6, snapshot_invalid=False
```

本机诊断页：`./bin/roomreverb serve` 后打开 `http://127.0.0.1:8088`。
