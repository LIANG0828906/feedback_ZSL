# StrokeForge

面向游戏 UI 字形装饰的可交互笔触填充与诊断工作台。纯本地、确定性、可重复演示：
固定种子生成含二次/三次贝塞尔轮廓的字形与解析真值，扁平化后生成笔触带，扫描线
像素化为灰度掩膜并统计密度，以笔画段级缓存、精确失效传播和版本化检查点保证
增量重绘与全量重绘逐像素一致。

## 能力总览

- **确定性样本与真值**：固定种子（默认 `20261125`）生成 3 个字形、每字形 2 条闭合轮廓、
  共 30 个笔画段；记录采样点坐标、闭合面积、包围盒、掩膜尺寸与笔画密度，不依赖外部字体或网络。
- **加载校验**：轮廓闭合、段首尾连续、段类型仅限 quadratic/cubic、坐标落在量化范围、
  笔画段按弧长比例无缺口覆盖 `[0,1]`。损坏 JSON/几何返回 `E_DECODE`，字段缺失返回 `E_SCHEMA`。
- **扁平化与笔触带**：de Casteljau 自适应细分（可配容差），按宽度、join、cap/nib 与沿骨架
  间距生成笔触带多边形与笔尖戳印；容差/宽度越界 `E_PARA`，越出外扩包围盒 `E_BOUND`。
- **扫描线像素化**：4×4 超采样的凸多边形半平面覆盖，逐像素取最大覆盖（顺序无关，支持并行重组）；
  掩膜尺寸由包围盒、padding 与缩放推导；密度为负/溢出返回 `E_DENS`，尺寸不符指出偏差字段。
- **段级依赖图与失效传播**：缓存键 = 轮廓内容哈希 + 段编号 + 笔触参数哈希 + 上游缓存键；
  参数变更只标记直接依赖段，控制点编辑只影响所属轮廓（验收实测 5 段失效、25 段复用），
  再沿 upstream DAG 传播到下游合成层，输出含段编号与原因的清单。
- **增量/全量一致性**：自动比对逐像素、逐采样点密度与提交序列，定位首个不一致像素坐标与段编号，
  不一致返回 `E_DIFF`。
- **段状态机**：`pending → processing → committed/failed`，`failed → pending`；
  非法迁移返回 `E_STATE` 且原状态不变；已提交段冻结，参数改动只作用于未提交段。
- **持久化检查点**：每提交一段即串行原子写（临时文件 + rename），记录掩膜分量、密度、
  内容/轮廓哈希、参数版本与算法版本；重启时任一哈希/版本不匹配则**整体作废、全量重绘**，
  绝不部分混用。
- **异常注入**：骨架自交 `E_SELF`、零长度段 `E_ZLEN`、密度溢出 `E_DENS`、单段内部失败
  `E_STAGE`，均回传字形与笔画段编号。
- **可视化诊断**：轮廓与骨架、笔触叠加、密度热力图、段状态矩阵、失效传播清单；
  渲染线程池限制并行度（默认 4）与每线程缓冲上限（默认 1 MiB）。

## 目录

```text
cmd/workbench/        demo（18 项验收）、serve（浏览器工作台）、generate
internal/model/       数据模型、量化类型与全部错误码
internal/loader/      JSON 解析与 E_DECODE/E_SCHEMA/E_PARA 校验
internal/generator/   固定种子样本与真值生成
internal/engine/      扁平化、笔触带、状态机、缓存键、失效传播、DAG 调度、异常注入
internal/raster/      扫描线超采样光栅化、掩膜合并与密度
internal/checkpoint/  原子串行检查点、版本/哈希校验、整体作废（pb 为无线环 wire 类型）
internal/service/     应用门面与诊断快照
frontend/dist/        无 Node 依赖的诊断界面（Wails 嵌入）
cmd/workbench/webroot 同一前端的浏览器工作台嵌入副本（scripts/sync-frontend.sh 同步）
scripts/              本地 sysroot 构建/运行与前端同步
```

## 快速开始

要求 Go 1.22+。核心引擎与浏览器工作台为纯 Go，`CGO_ENABLED=0` 即可运行：

```bash
go test ./internal/... -count=1
go run ./cmd/workbench demo -seed 20261125 -out artifacts
go run ./cmd/workbench serve -addr 127.0.0.1:8787
go run ./cmd/workbench generate -seed 20261125 -out artifacts
```

## Wails 桌面构建

标准 Ubuntu（已装 `build-essential`、`libgtk-3-dev`、`libwebkit2gtk-4.1-dev`）：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
wails doctor
wails build -tags webkit2_41
```

无 sudo 的本仓库环境：gcc 与 GTK/WebKit 依赖已解包在 `.sysroot/`，
`scripts/wails-build-local.sh` 会注入本地工具链后执行等价构建，产物为
`build/bin/strokeforge`。主机缺系统 WebKit 运行时目录时，
`bash scripts/run-local.sh` 通过等长路径补丁把 WebKit 子进程重定向到 `/tmp` 本地前缀运行，
不改系统目录、不使用容器。

## 错误码契约

| 码 | 触发条件 |
| --- | --- |
| `E_DECODE` | JSON 损坏、轮廓不闭合、段不连续、未知段类型、坐标越界、弧长覆盖缺口 |
| `E_SCHEMA` | 字段缺失、未知引用、枚举非法 |
| `E_PARA` | 宽度/容差/间距/缩放越界、并行度或缓冲超限 |
| `E_BOUND` | 笔触带超出包围盒外扩范围 |
| `E_DENS` | 密度为负/溢出、掩膜尺寸与真值不符 |
| `E_DIFF` | 增量与全量像素/密度/序列不一致 |
| `E_STATE` | 非法状态迁移、未提交即检查点化 |
| `E_SELF` / `E_ZLEN` / `E_STAGE` | 注入或真实的自交、零长度、阶段内部失败 |

## 设计要点

- **确定性并行**：就绪段在线程池内并行渲染，但发布到段缓存与提交序列按字形/段规范顺序进行，
  因此并行不改变可观测结果，全量与增量的提交序列天然相等。
- **顺序无关合成**：掩膜合并为逐像素 `max` 覆盖，配合 4×4 超采样，任意段渲染顺序得到同一掩膜。
- **轮廓级缓存键**：控制点编辑只改变所属轮廓哈希，其他轮廓段缓存键不变从而被精确复用。
- **故障隔离**：异常注入在单段渲染入口短路返回，诊断走单段路径，避免其他段错误抢先。

实际验证结果见 `artifacts/VALIDATION.md` 与 `artifacts/acceptance_report.json`。
