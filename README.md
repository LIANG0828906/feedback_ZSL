# IconBench 游戏 UI 图标前景/背景分离工作台

一个完全本地、确定性的 Go 工作台：固定种子生成游戏 UI 图标样本与像素真值，提供梯度/标记分水岭、RAG 区域合并、掩码清理、连通域统计、拖动粗预览、停止后精确重算、分块四层状态机、SQLite 原子检查点和独立验证工具。

- Go 1.22+，标准库 `net/http`，无前端构建、无外网资源。
- SQLite 驱动为纯 Go 的 `modernc.org/sqlite`，不需要安装数据库或 C 工具链。
- 推荐设置 `CGO_ENABLED=0`；Windows 可直接交叉编译出 `.exe`。
- 默认只监听 `127.0.0.1:8080`。

## Windows 快速演示

前置：安装 Go 1.22 或更高版本，并在“命令提示符”或 PowerShell 中确认：

```powershell
go version
```

在项目根目录执行一键脚本：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1
```

或使用 CMD：

```bat
scripts\demo.bat
```

脚本会依次执行：

1. `go mod download` 拉取 `modernc.org/sqlite`。
2. 生成确定性图标、像素参考掩码和真值清单到 `data/`。
3. 全量分块重算并写入 `data/checkpoints.db`。
4. 运行独立验证工具。
5. 启动页面并打开 `http://127.0.0.1:8080`。

也可以手动执行：

```powershell
$env:CGO_ENABLED="0"
go run ./cmd/workbench generate -data data
go run ./cmd/workbench recompute -data data
go run ./cmd/verify -data data
go run ./cmd/workbench -addr 127.0.0.1:8080 -data data
```

构建 Windows 可执行文件：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build-windows.ps1
.\bin\IconBench.exe -addr 127.0.0.1:8080
.\bin\verify.exe -data data
```

## 工作台操作

- 切换四张不同分辨率的确定性图标。
- 调整“淹没阈值”和“合并代价上限”，拖动时会进行 2×/3×/4× 降采样粗预览。
- 同时可调整最小区域面积、面积差上限、梯度算子、平滑核和开闭清理半径。
- 停止交互约 0.5 秒后，使用同一参数版本快照执行全精度分块重算。
- 页面展示预览降采样倍率、优先队列采样量、判定依据、IoU、重心/面积偏差、逐区域报告、判定历史、块/层提交序列和资源排队记录。
- 点击“独立验证持久层”会从 SQLite 的 `flood_merge` 层产物重建掩码并核对 `stats` 层。
- 点击“容差收紧曲线”会按 1.00/0.75/0.50/0.25 收紧预览容差，并把每次容差与实测值写入 `judgments` 表。

## 确定性样本与真值

生成器入口：`internal/sample/generator.go`。

固定种子为 `0x42A8691E`，生成：

| 图像 ID | 分辨率 | 真值连通域 |
| --- | ---: | ---: |
| `icon_grid_128` | 128×128 | 4（含 1 个孔洞环） |
| `icon_wide_192x128` | 192×128 | 5（含 1 个孔洞环） |
| `icon_tall_128x192` | 128×192 | 6（含 1 个孔洞环） |
| `icon_strip_256x128` | 256×128 | 7（含 1 个孔洞环） |

样本包含前景色块、同色系浅背景渐变、径向近色辉光、半透明内描边和确定性嵌入式噪声。每个形状由椭圆、圆角矩形、三角形和解析孔洞构造，再光栅化为像素参考掩码。

`data/manifest.json` 包含：

- 每个区域的重心、面积、外接框、孔洞数。
- 原始 PNG 与参考掩码 SHA-256。
- 自包含的 `reference_mask_png_base64`。
- 真值指纹 `truth_fingerprint`。
- 分块尺寸 `64`、工作线程预算 `4`、每线程像素缓冲上限 `65536`、预览延迟上限 `250ms`。
- 声明容差：掩码 IoU ≥ `0.93`、重心 ≤ `2.5px`、面积相对误差 ≤ `8%`、孔洞差 ≤ `0`。

加载器先用 PNG header 校验字段：损坏返回 `E_DECODE`；宽、高或通道数不一致返回 `E_META`，错误体含 `field` 和 manifest/file 两侧值。

## 算法结构

- `internal/imaging/`：RGB 转换、高斯/盒式平滑、Sobel/Scharr 梯度、降采样、最近邻放大、圆盘开闭运算、8 连通域、4 连通孔洞、IoU 和一对一重叠匹配。
- `internal/segment/markers.go`：边界像素拟合 RGB 线性背景平面，用颜色残差 Otsu 阈值得到前景/背景置信核心，腐蚀后生成稳定标记。
- `internal/segment/watershed.go`：以 `(priority, linear_index)` 作为确定性堆排序键做优先级淹没；相同输入不依赖 goroutine 调度或线程名。
- `internal/segment/merge.go`：构建区域邻接图，边代价为 `边界最小梯度 + 100 × 面积差 / 块像素数`；只合并相同种子类别的区域，并按静态边代价执行 Kruskal 式迭代合并。代价上限扩大只会增加可行边，因此上限收紧时区域数单调不减。
- `internal/pipeline/`：固定块切分、有界工作线程池、像素缓冲记账、四层任务编排、全局开闭清理和几何报告。
- `internal/checkpoint/`：SQLite schema、四态状态机、串行事务写入、哈希/版本校验和判定历史。
- `internal/verify/` 与 `cmd/verify/`：独立验证器和 CLI，只读取 manifest、SQLite 层产物和真值指纹来重建掩码。
- `internal/web/`：嵌入的原生 HTML/CSS/JS 与 `net/http` JSON API。

## 四层状态机

每个图像按固定块切分；每个块都有四层：

1. `gradient`
2. `markers`
3. `flood_merge`
4. `stats`

状态只能沿以下迁移推进：

```text
pending -> processing -> committed
                      \-> failed -> pending
```

非法迁移返回 `E_STATE`，事务回滚且原状态不变。进程崩溃留下的 `processing` 行会在下次启动时按 `processing -> failed -> pending` 复位后重算。

每次提交在同一 SQLite 事务中写：

- `checkpoints`：当前状态、层产物 JSON、摘要、RAG JSON、内容哈希、参数版本/哈希、算法版本、schema 版本和 `commit_seq`。
- `layer_commits`：单调提交序列。
- `state_events`：合法/非法状态迁移审计。

写入由 `sync.Mutex` 和 `SetMaxOpenConns(1)` 串行化。正常路径按“层顺序 → 块序号”提交，因此提交序列稳定。重启时只要任一内容哈希、参数版本、参数哈希、算法版本或 schema 版本不符，就删除该图像的全部检查点和提交序列并整体重算，不会混用旧块。

## 资源边界

`internal/pipeline/pool.go` 实现：

- 最大并行度 = manifest 声明的 `worker_threads`。
- 总像素令牌 = `worker_threads × pixel_buffer_per_thread`。
- 单任务像素数超过每线程上限返回 `E_RESOURCE`。
- 线程或像素缓冲不足时阻塞排队，并记录 `worker-thread-budget` 或 `pixel-buffer-exhausted`。
- 计算可并行；SQLite 提交串行，块/层提交顺序稳定。

## 预览与三条精确路径

拖动中的粗预览是真正的降采样近似：区域平均降采样、在缩小网格上运行同一标记/梯度/淹没规则，优先队列弹出量约按降采样平方减少，再最近邻放大。它会与精确结果做容差判定，默认预览容差为区域数差 `0`、重心 ≤ `3.5px`、面积相对误差 ≤ `14%`。

这里需要区分两个概念：

- **粗略预览掩码**：允许在声明容差内近似，页面记录实际倍率、PQ 采样量和依据。
- **交互停止后的预览路径最终精确结果**：与一次性全量重算、断点恢复重算使用同一个 `pipeline.RunImage`、同一参数版本快照、同一梯度算子和标记规则，因此掩码字节、统计和层提交键完全一致。

若粗略预览与精确重算的参数版本不同，返回 `E_VER`，错误中的 `version_a/version_b` 含参数整数版本、参数内容哈希和算法版本。超容差返回 `E_DIVERGE`，含区域数、最大重心偏差、最大面积相对误差、首个不一致区域和首个差异像素坐标。

## 错误码

| Code | 触发场景 | 关键字段 |
| --- | --- | --- |
| `E_DECODE` | PNG 损坏或参考掩码无法解码 | `field`, message |
| `E_META` | 宽/高/通道数与 manifest 不一致 | `field`, `expected`, `actual` |
| `E_PARA` | 参数越界或算子不支持 | `field`, `min`, `max` |
| `E_MERGE` | 合并迭代预算内未终止 | `remaining_pairs`, `min_cost` |
| `E_REGION` | 预测/真值区域不能一一匹配 | `extra[]`, `missing[]` |
| `E_DEGEN` | 面积为 0 或外接框退化 | `first_region_id` |
| `E_VER` | 预览/精确参数或算法版本混用 | `version_a`, `version_b` |
| `E_DIVERGE` | 粗预览超出一致性容差 | `metrics`, `first_region_id`, `diff_x`, `diff_y` |
| `E_STATE` | 非法块层状态迁移 | `expected`, `actual`, `layer` |
| `E_SNAPSHOT` | 检查点产物缺失/无法反序列化 | block/layer/field |
| `E_RESOURCE` | 单任务缓冲超过线程上限 | `field`, `expected`, `actual` |

## 常用 API

```powershell
curl http://127.0.0.1:8080/api/manifest
curl "http://127.0.0.1:8080/api/verify?id=icon_grid_128"
curl "http://127.0.0.1:8080/api/sequence?id=icon_grid_128"
curl "http://127.0.0.1:8080/api/judgments?id=icon_grid_128"
```

预览/重算请求体：

```json
{
  "image_id": "icon_grid_128",
  "factor": 2,
  "preview_param_version": 7,
  "params": {
    "param_version": 7,
    "gradient_operator": "sobel",
    "smooth_kernel": 3,
    "flood_threshold": 72,
    "min_region_area": 24,
    "merge_cost_cap": 42,
    "area_diff_cap": 2500,
    "morph_radius": 1
  }
}
```

## 测试与实际验证

运行全部测试：

```powershell
$env:CGO_ENABLED="0"
go test -count=1 ./...
```

测试覆盖：

- 样本生成、有效加载、`E_DECODE`、`E_META`。
- 同图同参数梯度和标记标签逐像素确定。
- 合并代价上限收紧时区域数单调不减，以及 `E_MERGE` 诊断。
- `E_PARA`、`E_DEGEN`、`E_REGION`。
- 非法状态迁移保持原状态、哈希不匹配检测。
- 资源池排队原因和超大缓冲 `E_RESOURCE`。
- `E_VER`、`E_DIVERGE`、容差收紧统计单调性。
- 全量、停止后精确预览、断点恢复三路径掩码字节、统计和提交序列一致。
- 篡改 SQLite 哈希后整体快照作废并重算恢复。

本机（Linux/Go 1.22.12，等价 Windows 纯 Go 构建）最近一次结果见 `docs/VERIFICATION.md`。
