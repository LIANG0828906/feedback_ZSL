# 实际验证记录

环境：Ubuntu 26.04，Go 1.22.12，gcc-15，CGO 启用；SQLite 驱动使用
`github.com/mattn/go-sqlite3 v1.14.53`（模块自带 amalgamation C 源码，无需系统 `sqlite3.h`）；
Node.js v18.20.4 本地解压于 `tools/`；Wails CLI v2.10.2。

## 自动化测试

```
CGO_ENABLED=1 go test ./...
```

结果：全部包通过（generator/loader/statemachine/checkpoint/errorsx/renderer/
soundfield/pipeline/workbench）。

竞态检测：

```
CGO_ENABLED=1 go test -race ./internal/pipeline ./internal/statemachine
```

结果：`ok`，无 data race。

静态检查：`go vet ./...` 无告警；`gofmt` 已格式化。

## 端到端验收

```
./bin/sir generate --out data
./bin/sir verify --dir data --sample stereo_48k     # verify OK: 8 frames
./bin/sir verify --dir data --sample quad_44k1      # verify OK: 7 frames
./bin/sir verify --dir data --sample surround_96k   # verify OK: 17 frames
```

三条路径（realtime/resume/replay）产出相同采样、相同延迟与能量统计、
相同误差上界与相同帧提交序列；渲染长度与原信号严格一致。

断点恢复：

```
./bin/sir run --sample stereo_48k --reset --mode realtime --pause 3  # committed=3/8
./bin/sir run --sample stereo_48k --mode resume                      # committed=8/8
```

异常链路（`sir faultdemo --kind ...`）实测：

| kind | 实测结果 |
| --- | --- |
| `chan` | `E_CHAN`，带声道标识 |
| `order` | 帧 1 `failed`，`E_ORDER`，其他 7 帧提交 |
| `clip` | 帧 1 `failed`，`E_CLIP`，其他 7 帧提交 |
| `frame` | 帧 1 `failed`，`E_FRAME`，其他 7 帧提交 |
| `bound` | 帧 0 `failed`，`E_BOUND` 且带指标名 |

失败帧解除注入后重试可重新提交（见 `TestFaultInjectionFrameIsolation`）。

参数与元数据错误：

```
./bin/sir run --sample stereo_48k --frame-hop 9999
# E_PARA, field=frame_hop
```

资源边界：`TestPoolBufferBlockingRecorded` 把缓冲预算压到单帧大小，
断言出现 `buffer_exhausted_blocked` 事件且无任务丢弃、全部帧最终提交。

面板：`sir serve --addr 127.0.0.1:18080` 实测 `/api/samples` 与 `/api/run` 正常返回。

桌面工程：`desktop/` 执行 `go build ./...` 通过；
Wails GUI 完整打包依赖 webkit2gtk 开发头文件，当前环境未安装，
因此桌面运行时以同构的内置 HTTP 面板交付。
