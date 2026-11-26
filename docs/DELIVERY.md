# 多光谱 / HDR 量化与重建验证工作台

本交付是一个仅依赖 Go 标准库的可集成领域组件与本地诊断工作台。所有数值主链路使用 `int64` 和 128 位中间乘积的整型运算，不使用浮点累积；样本由固定种子确定性生成。

## 快速运行

```bash
go test ./... -count=1
go run ./cmd/msdemo all --out artifacts/demo
go run ./cmd/msdemo serve --dir artifacts/demo --addr 127.0.0.1:8088
```

浏览器打开 `http://127.0.0.1:8088/workbench/index.html`。静态页面也可直接打开 `artifacts/demo/workbench/index.html`。

真实子进程强杀与恢复：

```bash
go build -o /tmp/msdemo ./cmd/msdemo
/tmp/msdemo crash --state /tmp/ms-sub --after 5   # 预期退出码 137 / SIGKILL
/tmp/msdemo resume --state /tmp/ms-sub
/tmp/msdemo full-run --state /tmp/ms-sub
/tmp/msdemo compare-reports -want /tmp/ms-sub/full-report.json -got /tmp/ms-sub/resume-report.json
```

## 包结构

- `internal/e`：稳定业务码 `E_DECODE/E_LOSSY/E_OVF/E_PARA/E_DRIFT/E_VER/E_DIFF` 与结构化错误信封。
- `internal/conv`：千分比/万分比、微秒/毫秒、ppm 增益定点换算；检查溢出并记录余数、截断位置和残差序列。
- `internal/domain`：统一光谱工程、块坐标、字段痕迹、损失清单和规范哈希。
- `internal/sample`：固定种子生成多波段线性辐射平面、曝光计数栈、逐块解析真值和 sparse/medium/dense 摘要。
- `internal/projectio`：三个 schema 版本的 DTO、加载器、导出器、默认值补齐、字段转换痕迹和降级损失清单。
- `internal/quant`：共享整型量化器；三档分别为逐像素精确、按行累计/块边界重算、分块累计/有界 LUT。
- `internal/recon`：波段响应与白点整型重建、线性色度、峰值统计、误差界和 `E_DRIFT` 判定。
- `internal/graph`：波段元数据/参数 → 块波段量化节点 → 块重建节点的双层依赖图、缓存键和精确失效清单。
- `internal/checkpoint`：每块每阶段单事务检查点；`FileStore` 是纯 Go 原子文件事务实现，`SQLiteStore` + `SQLiteSchema` 是 `database/sql`/SQLite 集成适配器。
- `internal/pipeline`：受限预算并行调度、全量、增量、崩溃恢复、三路径比对和端到端验证编排。
- `internal/workbench`：静态诊断界面与摘要。
- `cmd/msdemo`：生成、服务、强杀、恢复、全量和报告比对命令。

## 关键口径

- 内部量化刻度统一为万分比 `10000`，曝光统一为微秒，增益统一为 ppm， unity = `1_000_000`。
- 量化公式为 `q = floor((radiance * gain_ppm / 1e6) * exposure_us * 10000 / full_well_dose)`，饱和时钳制到 `10000`。
- 三档样本的观测数来自生成摘要：sparse=20、medium=40、dense=80（每块 16 像素 × 5 波段的不同抽样口径）。阈值可通过 `quant.Thresholds` 注入。
- dense LUT 以 64 辐射单位为粗桶；若桶两端量化值相同，由单调性保证命中同一整数商，若跨越量化边界则回退共享量化器。因此资源路径退化，但逐像素值仍一致。
- 缓存键为 `sha256(content_hash | band_id | param_version | algorithm_version | decision | upstream_cache_keys)` 的结构化 JSON 摘要。
- 检查点记录输出摘要、方式、误差界、缓存键、逐波段缓存键、内容哈希、参数版本和算法版本。

## SQLite 约束说明

任务同时要求“基于 SQLite”和“仅使用 Go 标准库、不安装第三方包”。Go 标准库只有 `database/sql` 接口，没有内置 SQLite 驱动。因此交付包含：

- 可在注册任意 SQLite 驱动后直接使用的 `checkpoint.SQLiteStore`；
- SQLite DDL：`internal/checkpoint/sqlite.go` 的 `SQLiteSchema`，演示产物也写到 `artifacts/demo/checkpoints/block_stage.sql`；
- 当前无第三方驱动环境下实际执行验收的 `checkpoint.FileStore`：临时文件、`fsync`、原子 `rename`、互斥串行提交和启动校验，提供与 SQLite 单事务相同的提交/恢复边界。

## 降级往返

导出 v1 时，标准 v1 字段不包含增益和嵌套树。导出器会生成 `E_LOSSY` 清单，同时把完整值写入 `x_ms_gain_ppm`、`x_ms_group_path` 等厂商保全扩展。v1 加载器优先恢复这些扩展；普通缺失字段则按声明默认值（unity 增益、`Ungrouped` 树）补齐并写转换痕迹。因此导出产物可被本仓库 v1 加载器读回并恢复等价内部语义，再次导出的损失项与摘要稳定一致。
