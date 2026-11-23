# 运行与验证手册

所有命令均在项目根目录、本地环境执行，不使用容器。SQLite 与快照写入项目内 `data/`。

## 1. 环境

- Go 1.22+（实测 `go1.22.12 linux/amd64`）。
- SQLite 选用纯 Go 驱动 `modernc.org/sqlite v1.29.10`，无需 CGO / MinGW，
  因此即使不配置 gcc 也可构建；当前环境 CGO 与 gcc 同样可用。
- 仅监听本地回环，无需账号、付费资源或 GPU。

## 2. 初始化与依赖（已完成）

```bash
go mod init occlbench
GOTOOLCHAIN=local go get modernc.org/sqlite@v1.29.10
```

## 3. 生成确定性样本

```bash
GOTOOLCHAIN=local go run ./cmd/generator -dir data/sample
```

产出 `data/sample/world.json`，默认布局：4 分区、16 分块、9 遮挡体、12 采样时刻，
并含逐时刻真值可见集合、参考遮挡体与屏幕投影真值。

## 4. 运行诊断工作台

```bash
GOTOOLCHAIN=local go run ./cmd/server -dir data/sample -db data/checkpoint -addr 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080/`。命令行访问注意本机若有代理需加 `--noproxy '*'`。

### HTTP API

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/api/snapshot` | 工作台快照（窗口矩阵、配置、缓存统计） |
| GET | `/api/chunks` | 分块着色、遮挡体、相机轨迹 |
| POST | `/api/preview` | 粗粒度预览（低分辨率栅格 + 抽样时刻） |
| POST | `/api/precise` | 精确剔除（全分辨率 + 全部时刻） |
| GET | `/api/verify` | 三路径逐窗口一致性比对 |
| POST | `/api/budget` | 调整槽位/内存预算（非法返回 E_BUDGET） |
| POST | `/api/quant` | 调整栅格分辨率（非法返回 E_PARA） |
| POST | `/api/retry` | 逐窗口重试 |
| GET | `/api/export` | 导出诊断 JSON |

## 5. 构建与测试

```bash
GOTOOLCHAIN=local go build ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go test ./...
```

## 6. 实际验证结果（本机记录）

### 6.1 单元/集成测试
`go test ./...` 全部通过，覆盖：
- 剔除引擎与独立射线真值逐时刻可见集合一致；
- 预览/精确共享视锥与量化、预览为保守子集；
- 并行剔除与串行剔除深度相等（纯函数、不受线程/顺序影响）；
- `E_SCHEMA/E_BOX/E_REF/E_PARA/E_STATE/E_BUDGET` 分支与定位字段；
- 状态机非法迁移保持原状态、失败窗口隔离与重试、已提交终态保护；
- 预算受限下未可见分块不抢占槽位、超预算分块进入延迟队列；
- 三路径逐窗口一致、断点恢复续算、版本失配整体作废后全量重算；
- 线程池并行度/栅格缓冲上限/取消；HTTP 路由与错误码。

### 6.2 三路径一致性
服务器启动日志与 `GET /api/verify` 均返回
`{"ok":true,"paths":["replay","realtime","resume"]}`，窗口数 4，逐窗口无差异。

### 6.3 版本失配整体作废
测试 `TestVersionMismatchInvalidatesWholeSnapshot` 修改槽位参数后，
恢复路径返回作废原因，清空全部旧窗口并全量重算，
重算结果与一次性全量重放逐窗口一致，未发生部分混用。

### 6.4 异常分支
`TestFailedWindowIsolationAndRetry` 验证单窗口加载失败仅置该窗口 `failed`，
相邻窗口保持 `pending` 不受污染，重试后经合法迁移回到 `committed`。

### 6.5 检查点落盘
SQLite `windows` 表窗口索引 0..3 严格连续、状态均为 committed、冻结标志为真、
轨迹消费偏移 3/6/9/12 递增；`meta.identity` 保存完整身份指纹。
