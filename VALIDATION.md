# 本地验收记录

日期：2026-11-27  
环境：Linux（WSL Ubuntu），`go version go1.22.12 linux/amd64`  
依赖：仅 Go 标准库；无 Docker、sqlite3、外部服务或网络账号。

## 1. 静态与测试检查

执行：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

实际结果：

```text
?   	stagepreview/cmd/stagepreview	[no test files]
ok  	stagepreview/stage	0.294s
ok  	stagepreview/web	0.177s

?   	stagepreview/cmd/stagepreview	[no test files]
ok  	stagepreview/stage	3.529s
ok  	stagepreview/web	2.571s

go vet ./...  # 无输出，退出码 0
```

共 19 个测试函数，覆盖：

- 三版本加载、v1 外锥/色温默认补全、未知版本与缺字段路径
- 相同种子的样本文件逐字节确定性
- `E_LOSSY` 分组/颜色降级与导出自校验
- 角度、tick/毫秒、千分比/万分比、色温、颜色整数换算与 `E_OVF`
- v3 -> v1 -> v3 姿态/亮度/颜色/淡入淡出时刻往返与声明残差
- 光锥真值、过渡带上下界、重叠累积、不同 worker 数一致性
- 零方向、倒锥、零采样距离、非法密度的错误上下文
- 双层状态机与非法迁移保持状态/进度
- 原子检查点续演、内容哈希篡改与异常 JSON 的整体 `E_VER`
- 渐变编辑的精确失效范围、下游摘要及未受影响窗口复用
- 依赖 DAG 的窗口范围传播
- 预览/精确共享点比对与首个 `E_DIVERGE` 定位
- 实时/断点续演/一次性全量三路径逐点一致
- 本地页面编辑、预览、精确预演、三路径验证和 JSON 状态端点
- 参数拖动用的不落盘 `/api/live-preview` 低密度统计端点

## 2. 确定性样本生成

执行：

```bash
go run ./cmd/stagepreview generate -out samples -seed 42
```

实际结果：

- schema：`stage.project.v1`、`stage-project/2`、`stage.project/3`
- 灯具：4 台
- 采样窗口：5 个（0/100/200/300/400 ms）
- 真值窗口：5 个
- 逐采样点真值：1960 点
- 内外锥过渡带不确定点：1440 点
- v1 默认补全：1 次（第一个灯具缺失外锥，按 `inner_half * 2.5` 补齐）
- v1/v2 分组扁平化降级：各 2 条 `E_LOSSY`

样本 SHA-256：

```text
truth.json  a58eb0e4e464d892183f2d349b722359136074ddf01112fd2d248c136c7337f8
v1.json     f40fb5eb75291e06c662510f62fd301cc97cee1a2462553594675461b1928dda
v2.json     3a9bf8c78523162ba220e9a9d52272d2cfcae03b6f0e60d5f726d1894234a269
v3.json     e7a504922a86e7f4c0b8379abee213be42f29ecc8d247f4034346f804a50f102
```

## 3. 统一验收命令

执行：

```bash
go run ./cmd/stagepreview verify -samples samples -runtime runtime/verify
```

机器可读报告已保存到 `validation/report.json`。实际结果：

```text
verify_ok True

schema_loads
  stage.project.v1 : fixtures=4 traces=62 defaults=1
  stage-project/2  : fixtures=4 traces=62 defaults=0
  stage.project/3  : fixtures=4 traces=51 defaults=0

truth
  windows=5 points=1960 uncertain=1440 algorithm=cone-kernel-v1

preview
  density=2 accumulation_bits=12
  compared_points=1000 ok=True within_budget=True elapsed_ms=0

three_paths
  live   commit_sequence=[1,2,3,4,5] result_hash=cb45f0adcdd0406094780a33901df5b07469269bee481c1ce66b1f674425a6e7
  resume commit_sequence=[1,2,3,4,5] result_hash=cb45f0adcdd0406094780a33901df5b07469269bee481c1ce66b1f674425a6e7
  full   commit_sequence=[1,2,3,4,5] result_hash=cb45f0adcdd0406094780a33901df5b07469269bee481c1ce66b1f674425a6e7
```

`elapsed_ms` 是实际运行值，会随机器负载变化；预览预算与证据字段在每次运行中重新记录。

## 4. 关键行为断言

- 光锥采样结果按采样时刻、灯具、坐标稳定排序；worker=1 与 worker=4 的完整结果逐点相同。
- 每个过渡带点都满足 `lower_bound < intensity < upper_bound`，并显式带 `uncertain=true`。
- 重叠光锥的聚合格至少包含一个多台灯具共同贡献的 cell，贡献为求和而非覆盖。
- 修改 F3 颜色渐变只失效采样时刻落在 200..400 ms 的窗口 3/4/5；窗口 1/2 从检查点复用。
- 篡改任一检查点的内容后，`LoadAll` 返回 `E_VER` 并拒绝整体恢复。
- 预览与全精度在共享采样点上强度、色度贡献和不确定标记一致；人工篡改精确结果强度后返回带坐标与灯具 ID 的 `E_DIVERGE`。
- 实时、断点续演、全量三条路径的逐窗口内容哈希、点集、聚合、不确定标记与提交序列一致。
- 非回环监听地址返回 `E_VAL`；页面只绑定回环地址。

## 5. 本地 HTTP 冒烟

实际启动编译后的服务并请求：

```text
GET    /api/status   -> 200
GET    /               -> 200，包含“舞台灯具阵列 · 预演诊断台”
POST   /preview        -> 303 -> /，包含“低密度预览证据”和“共享点比对”
POST   /api/live-preview -> 200，返回密度、精度、点数、延迟与预算判定
```

观察到：

```text
project stage-demo fixtures 4 schemas 3
source_version stage.project/3 algorithm cone-kernel-v1
evidence 9699 comparison 10147 bytes 38198
live preview: fixture=F1 density=2 bits=12 points=1000 fixtures=4 elapsed=1ms budget=250ms
page canvas: id="beam-preview" present
```

启动日志：

```text
2026/11/27 10:55:19 stagepreview listening on http://127.0.0.1:18548
```
