# MaskQA：图标掩码定点质检与增量精修组件

从零实现的 Go 领域组件和本地诊断工作台。所有图像、粗糙掩码和真值都由固定种子的确定性生成器产出；检查点使用 `database/sql` + `modernc.org/sqlite`，HTTP 工作台只依赖 Go 标准库 `net/http`。

## 快速验证

```bash
go mod tidy
go test ./... -count=1
go test -race . -count=1 -timeout 300s
go run ./cmd/maskqa generate -out artifacts/dataset.json
go run ./cmd/maskqa demo -dir artifacts
go run ./cmd/maskqa serve -addr 127.0.0.1:8080 -db artifacts/workbench.sqlite
```

浏览器打开 `http://127.0.0.1:8080`。工作台支持全量精修、受限并行精修、模拟 37 个提交后恢复、编辑迭代上限/半径带/梯度算子、查看失效清单和 PNG 偏差图。

最近一次本地验证：

- `go vet ./...`：通过。
- `go test ./... -count=1 -timeout 180s`：`ok maskqa 7.3s`，`cmd/maskqa` 无测试文件。
- `go test -race . -count=1 -timeout 300s`：`ok maskqa 93.5s`。
- `go run ./cmd/maskqa demo -dir artifacts`：180 个稳定提交节点，`three_paths_identical=true`，迭代上限编辑产生 60 个精确失效节点。

默认种子样本的参考掩码偏差（像素数）：

| 样本 | 初始偏差 | 精修后偏差 | 真值连通域 |
|---|---:|---:|---:|
| `icon-blobs-96x72` | 38 | 28 | 2 |
| `icon-ring-80x80` | 36 | 26 | 1（含 1 个真值孔） |
| `icon-tri-112x80` | 37 | 33 | 2 |

偏差图中绿色为正确前景、红色为精修相对真值的外溢、橙色为内缩。

## 领域能力与代码位置

- 确定性样本和真值统计：`generate.go`
  - 3 张不同分辨率图标；近色渐变背景、前景色块、半透明描边、嵌入式噪声。
  - 粗糙掩码包含边界内缩、外溢、孤立小孔和外部碎块。
  - 真值连通域给出重心、面积、外接框和孔洞数。
- 装载 schema 校验：`validate.go`
  - 尺寸、RGBA/掩码长度、半径带、迭代上限、真值区域和统计一致性。
  - 错误为结构化 `QAError`，包含 `code/field/object_id`，失败码 `E_SCHEMA`。
- 定点梯度和边缘带：`gradient.go`、`edge.go`
  - 支持 `sobel3`、`prewitt3`、`sobel5`。
  - RGB 亮度权重、Sobel 乘加、欧氏模长和刻度除法均记录商、余数、截断位置。
  - 全局排列分位阈值和确定性膨胀生成边缘带；相同参数下不依赖块顺序或调度。
- 曲率分段半径：`radius.go`
  - 确定性边界追踪、固定弧长分段、Menger 曲率定点值、排名分箱。
  - 高曲率段映射到不更大的半径；每段记录曲率、分箱、腐蚀/膨胀半径和映射依据。
  - 非法曲率返回 `E_RADIUS`，带边界段 ID 和实测曲率。
- 分段半径形态学：`morphology.go`
  - 自适应圆盘膨胀/腐蚀、闭运算、开运算和交替轮次。
  - 每轮记录 close/open/整轮变化像素；`round_changes <= change_threshold` 才收敛。
  - 耗尽 `max_iterations` 返回 `E_ITER`，包含当前轮次和最小变化数。
  - 输出保持原分辨率单通道；所有坐标先做边界检查，不会写越界位置。
- 五级误差溯源：`report.go`、`evidence.go`
  - 级：`gradient`、`threshold`、`radius`、`morphology`、`composition`。
  - 每像素保留观测余数、分母、归一化微单位贡献和截断表达式。
  - 输出五级贡献总和、相对真值掩码偏差像素/比率、每级最大残差位置和全局最大位置。
- 状态机和 SQLite：`state.go`、`store_sqlite.go`
  - 状态：`pending`、`processing`、`committed`、`failed`。
  - 合法迁移：`pending→processing`、`processing→committed/failed/pending`、`failed→pending`、`committed→pending`（仅失效/重算）。
  - 非法迁移返回 `E_STATE` 且保持原状态。
  - 每个块阶段在同一事务写 `commits` 和 `block_state`；写入互斥、单连接，提交顺序稳定。
  - 重启校验内容哈希、算法版本和阶段参数版本；首个不匹配点及其后提交整体删除，并从剩余提交重建状态。
- 缓存键和增量失效：`service.go`
  - 缓存键输入：块内容哈希、样本/块/阶段、边界段半径值哈希、阶段参数版本、上游缓存键。
  - 失效项包含样本、块坐标、阶段、原因、参数字段和上游来源。
  - 迭代上限编辑只失效 `open`、`compose`（30 块 × 2 = 60 节点）。
  - 半径带编辑失效 `radius/close/open/compose`（120 节点），复用梯度和边缘带。
  - 梯度算子编辑使梯度及其全部下游依赖失效（180 节点），但仍输出明确依赖链而非清空数据库。
- 三路径和调度：`run.go`、`scheduler.go`、`paths_test.go`
  - 一次性全量、受限并行实时、37 提交中断后续跑三条路径比对掩码、连通域统计和内容哈希提交序列。
  - `CompareRunReports` 返回 `E_DIFF`，定位首个不一致的样本、块坐标、阶段和字段。
  - 调度器限制最大并行度、每线程像素缓冲和待处理像素预算；预算耗尽时阻塞并记录等待原因/时长，不丢任务。
  - 单块阶段失败只置该节点失败；其他块可提交，重试走 `failed→pending→processing`。

## HTTP API

- `GET /`：诊断工作台。
- `GET /api/state`：参数、样本偏差、提交数、调度指标、失效清单和全局最大残差。
- `POST /api/action`：`{"action":"run|realtime|resume-demo|edit-iter|edit-radius|edit-operator|reset"}`。
- `GET /api/trace?sample=<id>&x=<x>&y=<y>`：单像素五级溯源。
- `GET /png?sample=<id>&kind=image|rough|truth|final|diff`：标准库 PNG 输出。

## 可集成 API 示例

```go
package main

import (
    "context"
    "fmt"
    "maskqa"
)

func main() {
    dataset := maskqa.GenerateDataset()
    svc, err := maskqa.NewService(dataset, "checkpoints.sqlite")
    if err != nil { panic(err) }
    defer svc.Close()

    report, err := svc.Run(context.Background(), maskqa.RunOptions{
        Mode: "full",
    })
    if err != nil { panic(err) }

    for _, sr := range report.SampleReports {
        fmt.Println(sr.SampleID, sr.ErrorReport.ReferenceMismatchPixels)
    }
}
```

## 错误码

| Code | 触发场景 |
|---|---|
| `E_SCHEMA` | 数据集字段、尺寸、半径带、迭代上限或真值区域不合法 |
| `E_PARA` | 梯度算子尺寸、分位刻度、调度预算等参数越界 |
| `E_RADIUS` | 曲率无法映射为声明半径带内的正整数 |
| `E_ITER` | 形态学交替轮次在迭代上限内未收敛 |
| `E_STATE` | 非法块阶段状态迁移 |
| `E_DIFF` | 三条路径的掩码、统计或提交哈希不一致 |
| `E_CACHE` / `E_STORAGE` | 缓存或 SQLite 检查点故障 |

## 设计说明

默认半径带刻意保守为 `[1,1]`，使合成的 1px 级内缩、小孔和碎块可稳定修复且不明显改动真实边缘。需要观察曲率半径分级时，可在工作台“切换半径带”，或在代码中设置 `Params.RadiusMin=1`、`RadiusMax=4`；`TestRadiusMonotonicAndError` 验证高曲率到小半径的单调映射。

五级贡献使用统一微单位：`observed_remainder * trace_scale / observed_denominator`。这些数值是定点量化残差强度，不把不同量纲的误差伪装成像素数；报告同时给出相对真值的真实像素偏差用于精度对照。
