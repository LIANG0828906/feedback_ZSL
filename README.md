# 舞台灯光预演内核（stagepreview）

这是一个可独立集成、也可通过本地页面诊断的 Go 领域组件。项目仅使用 Go 1.22 标准库，不依赖真实灯具、外部服务、Docker、第三方包或 SQLite。

当前仓库包含：

- 固定种子的确定性样本生成器与三套 schema 工程文件
- 统一 schema 加载、内部模型映射、逐字段转换痕迹与默认补全
- 显式版本升降级、`E_LOSSY` 降级清单与导出自校验
- 整数角度/时间/颜色/色温换算、溢出与余数记录
- 逐采样时刻灯具姿态求值、三维光锥采样、重叠贡献累积
- 双层状态机、基于标准库文件的原子检查点与全量恢复校验
- 依赖 DAG、精确编辑失效与检查点复用
- 低密度预览/全精度预演双路径、实时/续演/全量三路径一致性验证
- 回环地址上的 `net/http` 诊断页面与 JSON 诊断端点

## 快速开始

```bash
go test ./...
go run ./cmd/stagepreview generate -out samples -seed 42
go run ./cmd/stagepreview verify -samples samples -runtime runtime/verify
go run ./cmd/stagepreview serve -addr 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080/`。页面可重新生成样本、拖动灯具参数和颜色渐变并即时重绘俯视采样点画布、执行预览/精确预演、查看失效清单、检查点、三路径比对与完整 JSON 诊断快照。

也可以使用 Makefile：

```bash
make test
make race
make generate
make verify
make serve
```

## 目录

| 路径 | 内容 |
|---|---|
| `stage/` | 可集成领域包：模型、schema、换算、预演、采样、状态、检查点、依赖与验证 |
| `cmd/stagepreview/` | `generate`、`verify`、`serve` 命令 |
| `web/` | 本地诊断页面、HTTP 路由与页面流程测试 |
| `samples/` | 固定种子生成的 `v1.json`、`v2.json`、`v3.json`、`truth.json`、`manifest.json` |
| `validation/report.json` | 最近一次本地验收的机器可读结果 |
| `VALIDATION.md` | 实际执行命令、结果与环境记录 |

`runtime/` 是页面检查点、当前编辑状态和三路径验证目录，已加入 `.gitignore`。

## 确定性样本

默认种子为 `42`。生成器先构造内部工程，再从同一内部工程导出三种 schema：

| 版本 | 标识 | 阵列组织 | 角度 | 时间 | 颜色/色温 | 光锥 |
|---|---|---|---|---|---|---|
| v1 | `stage.project.v1` | 扁平 `fixtures[]` | 0.1° 整数 `ddeg` | 120 Hz `tick` | RGB 千分比，CCT 千分比 | 全角，`outer_ddeg` 可缺失 |
| v2 | `stage-project/2` | 扁平 `fixtures[]` | 微弧度 `urad` | 微秒 `us` | `#RRGGBB`，CCT Kelvin | 半角 `urad`，外锥必填 |
| v3 | `stage.project/3` | `rigs[].groups[].fixtures[]` | 毫度 `mdeg` | 毫秒 `ms` | RGB 万分比，CCT 万分比 | 半角 `mdeg` |

v1 缺失外锥时按声明默认 `inner_half * 2.5` 补齐；缺失色温时按声明默认 `5600 K` 补齐。每次补全都会生成 trace，记录字段路径、`<missing>` 旧值、内部值和默认来源。v1/v2 不支持 v3 的 rig/group 结构，导出时保留全部灯具与光学字段，但为每个分组生成带处置方式的 `E_LOSSY` 项。

样本规模为 4 台灯具、5 个采样窗口、逐采样点真值 1960 点，其中 1440 点位于内外锥过渡带并带有上下界。`truth.json` 给出每个窗口、采样时刻、灯具、采样点的坐标、距离、强度、色度贡献与不确定标记，以及聚合格的重叠贡献。

## 领域 API

```go
package main

import (
	"context"
	"os"

	"stagepreview/stage"
)

func preview(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	project, err := stage.LoadProject(data)
	if err != nil {
		return err // E_SCHEMA/E_PARA/...
	}
	run, err := stage.RunProject(context.Background(), project, stage.RunOptions{
		Workers:  4,
		Sampling: stage.ExactSampling(),
		Store:    stage.NewCheckpointStore("runtime/checkpoints"),
	})
	if err != nil {
		return err
	}
	_ = run.Windows
	return nil
}
```

常用入口：

- `GenerateProject(seed)` / `GenerateSampleBundle(seed)`：确定性内部工程与全部样本产物
- `LoadProject(data)`：自动识别 v1/v2/v3，统一映射为内部模型
- `ExportProject(project, version)` / `ConvertProject(data, version)`：显式升降级
- `RunProject(ctx, project, options)`：并行窗口计算、稳定顺序提交、检查点复用
- `CompareProjects(old, new)`：编辑差分与精确窗口失效清单
- `BuildDependencyGraph(project, sampling)`：位置、锥角、指令、姿态、采样和窗口摘要 DAG
- `RunPreview` / `ComparePreviewWithExact`：低密度预览与全精度共享点比对
- `VerifyThreePaths` / `CompareRunResults`：实时、断点续演、全量三路径逐点比对
- `CheckpointStore.LoadAll`：全有或全无的恢复校验

错误均为 `*stage.Error`，带稳定的 `Code` 与所需上下文；可用 `errors.Is(err, stage.ErrParam)` 或 `stage.HasCode` 判断。

| 错误码 | 场景 |
|---|---|
| `E_SCHEMA` | 未知版本、必填字段缺失、结构校验失败 |
| `E_LOSSY` | 低版本无法表达分组或颜色精度，但仍返回可读回产物 |
| `E_OVF` | 整数换算溢出，包含字段与两个操作数 |
| `E_PARA` | 零指向向量、内外锥倒置等参数错误 |
| `E_VAL` | 采样距离为零、密度越界、非法聚合参数 |
| `E_STATE` | 非法状态迁移，原状态与进度保持不变 |
| `E_VER` | 检查点内容哈希、参数版本、算法版本或提交序列不匹配 |
| `E_DIVERGE` | 预览/精确不一致，或三路径不一致，带首个窗口、灯具、字段与坐标 |

## 数值与确定性

内部单位统一为：

- 位置：微米
- 角度：微度
- 时间：毫秒
- 亮度与 RGB：万分比
- 色温：Kelvin

所有 schema 单位换算使用 `int64` 与 `math/big` 整数运算，不经浮点累积。`ScaleInt` 显式声明 `nearest`、`floor`、`ceil` 或 `toward_zero` 取舍方向并返回有符号余数。角度/弧度换算使用声明常量 π = `103993/33102`；微弧度表示量化到 1 µrad，往返最坏残差低于 30 µ度（0.00003°）。时间 tick/毫秒、千分比/万分比色温和亮度、8 位颜色与万分比颜色都有独立换算函数。

声明的目标格式量化上界：

- v1 的 `ddeg` 姿态/锥角量化残差 ≤ 50,000 µ度（0.05°）。
- v1 的 `permille` 亮度/RGB 与内部 `permyriad` 往返残差 ≤ 5 个万分比单位。
- v2 的 `#RRGGBB` 每通道往返残差 ≤ 20 个万分比单位。
- v1 120 Hz tick 与毫秒按最近取整，往返时间残差 ≤ 4.17 ms；v2 微秒与毫秒残差 ≤ 0.5 ms。
- v3 毫度表达以 1,000 µ度为步长，内部姿态不是该步长整数倍时残差 ≤ 500 µ度（0.0005°）。
- CCT 千分比/万分比表达分别覆盖声明的 1800..12000 K 范围，按对应刻度最近取整。

姿态插值也使用整数除法并向零截断；几何方向只从已确定的内部整数姿态一次性转换为 `float64` 用于三角函数和射线构造，不跨帧累积浮点误差。窗口并行只改变计算调度，不改变排序后的提交、点集合、聚合键或累加顺序。

## 采样与累积

光锥在声明距离处构造截面：中心点、径向环与方位点。使用微弧度/毫度输入时仍以未舍入截面半径判定过渡带：

- 内锥内：确定点，强度为当前亮度。
- 内外锥之间：`Uncertain=true`，强度取中点，上下界为 `[25%, 100%]`。
- 外锥外：不加入有效采样集。

采样点先按 `(distance, x, y, z)` 去重排序，再按空间格聚合。重叠光锥的贡献按灯具 ID 稳定排序后求和，不覆盖已有贡献；聚合格显式保存 `Contributors`。全精度配置使用 2 个截面距离、4 个径向环、12 个方位段、300 mm 聚合格和 32 bit 精度；预览降为 2 个径向环、900 mm 聚合格和 12 bit 精度。聚合精度映射到 0..10000 内部刻度：32 bit 保持 `step=1`，12 bit 使用 `step=3` 的整数向下量化。预览报告记录实际密度、精度、延迟和判定依据。

## 状态与检查点

采样窗口状态：

```text
pending -> pose_evaluating -> cone_sampling -> committed
        \-> failed
```

灯具状态：

```text
pending -> evaluated -> sampled -> committed
        \-> failed
```

非法迁移返回 `E_STATE`，不改变当前状态或进度。每个窗口提交时，检查点包含逐灯具采样摘要、强度/色度累积、依赖集合、内容哈希、参数版本与算法版本。写入流程为：同目录临时文件写入、`fsync`、`rename`、目录 `fsync`；`CheckpointStore` 互斥量保证写入不交错，`RunProject` 始终按窗口号稳定顺序提交。

恢复时所有 `window-*.json` 必须组成连续序列，文件名、窗口号、版本、内容哈希全部一致，否则整体返回 `E_VER`，不会做部分恢复。

## 失效传播

依赖 DAG 显式列出：

- `fixture:<id>:position|cone|orientation|color|level|content`
- `cue:<id>`
- `fixture:<id>:window:<n>:time:<t>:pose|sample`
- `window:<n>:summary|aggregates`

灯具缓存键由灯具内容哈希、采样时刻、参数版本和上游指令缓存键共同决定。静态位置/方向/锥角/颜色修改影响该灯具使用的全部窗口；颜色或淡入淡出指令修改只影响采样时刻落在旧/新增指令区间并集的窗口。未受影响窗口直接复用检查点。`RunProject` 可通过 `RunOptions.Previous` 返回精确的窗口号、灯具/指令和下游摘要列表；页面编辑同时显示 `CompareProjects` 的精确差分。

## 本地页面

服务只接受回环地址，`0.0.0.0` 等非回环绑定会返回 `E_VAL`。主要路由：

| 路由 | 作用 |
|---|---|
| `GET /` | 工程、灯具、节目单、窗口、预览、检查点与失效诊断 |
| `GET /api/status` | 当前工程、最近运行、预览/精确比对、三路径结果 JSON |
| `POST /api/live-preview` | 参数拖动期间的低密度、不落盘实时预览与统计 |
| `POST /generate` | 按固定种子重新生成 schema 与真值 |
| `POST /preview` | 低密度预览并对共享点执行精确比对 |
| `POST /exact` | 全精度预演，复用未失效检查点 |
| `POST /verify` | 实时/续演/全量三路径比对 |
| `POST /fixture` | 编辑姿态、锥角、亮度、色温与 RGB |
| `POST /cue-color` | 编辑颜色渐变起止值 |
| `POST /reset` | 恢复样本 v3 初始状态 |

编辑后的工程原子写入 `runtime/current-v3.json`，重启页面仍可恢复；检查点位于 `runtime/checkpoints/`。

## 验证

实际执行结果见 `VALIDATION.md` 与 `validation/report.json`。核心验证覆盖：

- 三版本加载、v1 默认补全、转换 trace、未知版本/缺字段错误
- 降级 `E_LOSSY` 与读回结构校验
- 角度、时间、色温、亮度、颜色整数换算与溢出
- 光锥采样真值、过渡带上下界、重叠累积、不同 worker 数一致性
- 零方向/倒锥/非法密度错误上下文
- 双层状态机非法迁移
- 检查点续演、内容哈希篡改后整体 `E_VER`
- 颜色渐变编辑只失效窗口 3/4/5，窗口 1/2 复用
- 预览/精确比较与实时/续演/全量三路径逐点一致
- 页面 GET/编辑/预览/精确预演/状态 API 流程

命令：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go run ./cmd/stagepreview verify -samples samples -runtime runtime/verify
```
