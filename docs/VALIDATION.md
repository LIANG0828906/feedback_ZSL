# 实测记录

环境：Linux/WSL2，Go `go1.22.12`，`modernc.org/sqlite v1.34.5`，CGO 不参与 SQLite 驱动实现。测试日期：2026-11-25。

## 1. 单元/集成测试

```text
$ go test -race ./...
?   	example.com/multiscreen-cast/cmd/castctl	[no test files]
?   	example.com/multiscreen-cast/cmd/castgen	[no test files]
ok  	example.com/multiscreen-cast/kernel	1.062s
ok  	example.com/multiscreen-cast/protocol	1.014s
ok  	example.com/multiscreen-cast/samplegen	1.015s
ok  	example.com/multiscreen-cast/store	1.186s
ok  	example.com/multiscreen-cast/verify	1.793s

$ go vet ./...
(no output)
```

覆盖点：

- 合法样本校验，以及 `E_SCHEMA/E_REF/E_ORDER/E_CAP` 的来源与字段路径；
- 非法状态指令不消费、不改变状态与进度；
- 已结束会话拒绝后续指令；
- 同刻并发加入裁决、加入回放、同屏并发冲突；
- 整数映射、裁剪方向、`E_PARA` 屏幕/字段定位；
- 工作池排队记录与每线程像素缓冲上限；
- SQLite 检查点顺序、断点恢复、单行损坏整体失效；
- 内容哈希/拓扑版本/算法版本不匹配整体失效；
- 生成器固定种子确定性；
- 真值、三路径、四类异常链路。

## 2. 主样本验证

```bash
go run ./cmd/castgen -seed 42 -screens 4 -windows 12 -sessions 2 -out samples/sample.json
go run ./cmd/castctl verify -sample samples/sample.json -workdir .cast-verify
```

结果：

```json
{
  "ok": true,
  "report": {
    "sessions": 2,
    "windows": 24,
    "truth_matched": true,
    "three_paths_matched": true,
    "recovery_points_checked": 24,
    "realtime_windows": 24,
    "replay_windows": 288,
    "checkpoint_rows_checked": 312,
    "forced_stop_exit_report": false
  },
  "anomalies": {
    "decode": "E_DECODE",
    "order": "E_ORDER",
    "cap": "E_CAP",
    "segment": "E_SEG"
  }
}
```

解释：

- 2 个会话，每个 12 个窗口，共 24 个已提交窗口；
- 每个会话检查 1..12 全部恢复断点，共 24 个恢复点；
- 恢复路径累计重放 288 个窗口；
- 实时/恢复路径累计检查 312 条检查点行；
- 真值与三路径一致；
- 强制终止场景不生成退出报告。

## 3. 小窗口边界

```bash
go run ./cmd/castgen -seed 8 -screens 4 -windows 2 -sessions 1 -out /tmp/cast-small.json
go run ./cmd/castctl verify -sample /tmp/cast-small.json -workdir /tmp/cast-verify-small
```

```json
{
  "ok": true,
  "report": {
    "sessions": 1,
    "windows": 2,
    "truth_matched": true,
    "three_paths_matched": true,
    "recovery_points_checked": 2,
    "realtime_windows": 2,
    "replay_windows": 4,
    "checkpoint_rows_checked": 6,
    "forced_stop_exit_report": false
  }
}
```

该样本覆盖最后窗口之后仍有加入/退出的情况，验证会话终态端集合并未错误取自最后窗口快照。

额外参数矩阵：

```text
matrix PASS: 40 combinations (screens=1..8, windows=1,2,3,5,8)
```

每一组均重新生成样本并执行真值、实时、全量重放和逐窗口恢复比对。

## 4. 实际断点恢复命令

```bash
go run ./cmd/castctl run -sample samples/sample.json -mode realtime \
  -db /tmp/cast-progress.sqlite -session session-000042-00
go run ./cmd/castctl run -sample samples/sample.json -mode recovery \
  -db /tmp/cast-progress.sqlite -session session-000042-00
```

```json
{
  "ok": true,
  "mode": "realtime",
  "restored": false,
  "session_id": "session-000042-00",
  "windows": 12,
  "final_state": "ended"
}
{
  "ok": true,
  "mode": "recovery",
  "restored": true,
  "session_id": "session-000042-00",
  "windows": 12,
  "final_state": "ended"
}
```

## 5. 异常注入命令

```bash
go run ./cmd/castctl inject -sample samples/sample.json -kind order -out /tmp/cast-order.json
go run ./cmd/castctl verify -sample /tmp/cast-order.json -anomalies=false
```

返回码为 1，并定位首个异常：

```json
{
  "ok": false,
  "error": {
    "code": "E_ORDER",
    "source": "/tmp/cast-order.json",
    "path": "$.sessions[0].windows[1].time",
    "message": "window time must be monotonic: 20 after 21",
    "window": 2
  }
}
```

其余注入：

```bash
castctl inject -kind decode
castctl inject -kind cap
castctl verify                 # E_SEG 由 FailureInjector 在验证过程中触发
```

实测定位示例：

```json
{
  "code": "E_CAP",
  "source": "/tmp/cast-cap.json",
  "path": "$.sessions[0].displays[0].capability.resolution",
  "message": "capability resolution 1921x1080 conflicts with topology resolution 1920x1080",
  "display_id": "screen-a"
}
```

```json
{
  "code": "E_SEG",
  "source": "engine",
  "path": "$.windows[0]",
  "message": "injected window failure: deterministic segment fault",
  "time": 10,
  "window": 1,
  "display_id": "screen-a",
  "field": "window"
}
```

## 6. 本机能力

```json
{
  "ok": true,
  "go_version": "go1.22.12",
  "working_dir": "/home/liang/feedback/uid42a8691e-default-245",
  "sqlite_writable": true,
  "loopback": true,
  "loopback_address": "127.0.0.1:44330"
}
```

`doctor` 在真实工作目录创建并打开 SQLite 文件，同时绑定 `127.0.0.1` 临时端口；Windows 下使用相同 Go 工具链和纯 Go SQLite 驱动即可，不依赖 Docker 或外部服务。
