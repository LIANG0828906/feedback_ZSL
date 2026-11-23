# 正射校正领域组件 — 技术说明

模块 `ortho`（仅标准库，Go 1.22+），面向游戏地形纹理/贴图的三阶段正射校正：
透视单应 → 径向畸变校正 → 仿射缩放平移。不依赖任何外部网络服务。

## 结构

| 文件 | 职责 |
|---|---|
| `errors.go` | 稳定错误码 E_PARA / E_DECODE / E_GEOM / E_BAND / E_DIFF / E_STAGE / E_STATE |
| `geo.go` | 三阶段变换（正向/反向映射、参数范围校验、输出尺寸推导） |
| `raster.go` | PNG 解码（E_DECODE）、内容哈希、单/三通道逐通道采样 |
| `resample.go` | 分块重采样、工作池、像素缓冲上限与等待事件记录 |
| `cache.go` | 阶段缓存、提交守卫（E_STATE）、提交顺序 |
| `pipeline.go` | 依赖链、缓存键、失效清单、增量调度、逐像素比对 |
| `truth.go` | 解析真值加载与逐阶段重投影误差校验（E_GEOM） |
| `persist.go` | 原子快照读写、版本/哈希识别、整体作废 |
| `samplegen.go` | 固定种子（42）确定性样本/参数/真值生成器 |
| `overlay.go` | 控制点叠加图与差异热力图 |
| `cmd/demo` | 可运行示例（命令行报告 + out/ 图像产物） |

## 关键设计

- **缓存键**：`key_i = SHA256(stageID, paramVersion, canonicalParams, key_{i-1})`，
  `key_0 = SHA256("ROOT", sourceContentHash)`。源图内容哈希经链根键逐段传递，
  任一阶段参数变化使其自身及全部下游键失效。
- **失效清单**：`Plan()` 输出每阶段 `reused / invalidated(reason)`，
  原因取 `cache_miss`、`params_or_input_changed`、`upstream_invalidated`。
- **分块合并**：每个输出像素取值只依赖源图与参数，块只写自身精确矩形，
  边带仅界定源图取数窗口，故分块并行 / 整图单次 / 串行结果逐像素一致。
  边带下限随插值阶数联动：`RequiredBand = order + 1`，不足返回 E_BAND 并给出最小宽度。
- **径向求逆**：正向解析式 `p' = c + (p-c)(1+K1 r²+K2 r⁴)`，反向用固定 50 次
  牛顿迭代（确定性），残差 < 1e-6 px。
- **资源边界**：工作池上限 `Workers`；像素缓冲预算 `BufferPixels` 折算为在飞块
  缓冲数，耗尽时阻塞排队并记录 `pixel_buffer_exhausted` 等待事件。
- **快照**：阶段 PNG（临时文件 + rename）先于 `snapshot.json` 原子提交点写入，
  互斥锁串行化；加载时校验参数版本、源图哈希、各阶段文件内容哈希，
  任一不匹配即整体删除快照目录，禁止部分混用。
- **提交守卫**：已提交且未显式失效的阶段项拒绝改写（E_STATE）；
  阶段失败仅标记该阶段 `failed`，其余缓存保留，可修正后重试。

## 验证结果（go1.22.12 linux/amd64）

- `go vet ./...`：通过。
- `go test -race ./...`：全部通过（约 9s），覆盖：
  变换往返精度、参数越界 E_PARA（含字段名）、E_DECODE、分块=整图、
  并行=串行（图像与提交顺序）、通道数保持、E_BAND 最小宽度、
  缓冲耗尽等待记录、增量=全量、E_DIFF 首个不一致像素定位、
  提交守卫、阶段失败重试、真值校验通过与篡改后 E_GEOM（含超差点编号）、
  快照往返/版本不匹配/源哈希不匹配/文件损坏整体作废、样本逐字节确定性。
- `go run ./cmd/demo`：退出码 0，输出见 `demo_output.txt`；产物在
  `samples/`（地形图、stages.json、truth.json）与 `out/`（各阶段图、
  控制点叠加、差异热力图、快照目录）。真值校验三阶段 median=max=0.000000
  （容差 0.5px）；增量与全量逐像素一致（热力图 diff=false）；
  小缓冲运行记录 68 次缓冲等待事件且结果仍逐像素一致。
