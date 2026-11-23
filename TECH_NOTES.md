# 编解码质量评估管线 — 技术说明

## 1. 模块结构（纯 Go 标准库，无第三方依赖）

| 包 | 职责 |
| --- | --- |
| `internal/errs` | 七个业务错误码 `E_SCHEMA/E_DECODE/E_PARA/E_STATE/E_BAND/E_BOUND/E_DIVERGE` 与稳定整数退出码 |
| `internal/fixed` | 声明精度的 Q16.16 定点乘除、开方与截断追踪 |
| `internal/sample` | 确定性 LCG 样本生成、8×8 正交 DCT 编解码器、真值清单、二进制帧格式与加载校验 |
| `internal/metrics` | 参数快照、定点 SSIM 窗口网格、PSNR、块级整数累加统计、边缘保真度 |
| `internal/propagation` | 系数量化误差→像素误差传播、块边界跃变、解析误差上界与 `E_BOUND` 击穿 |
| `internal/statemachine` | 待评估→评估中→已提交/失败 状态机，非法迁移 `E_STATE` 且保持原态 |
| `internal/store` | 追加式块检查点（JSON 行 + CRC32 + fsync）、原子元数据、整快照作废 |
| `internal/pipeline` | 有界并发池、确定性窗口归属、三路径编排、纯函数三路径比对 |
| `internal/verifier` | 只读独立验证器，仅用持久化块与真值清单复算 |

命令行入口位于 `cmd/generator`、`cmd/pipeline`、`cmd/compare`、`cmd/verify`。

## 2. 确定性样本与真值

- 随机源为固定种子 LCG；原始帧 = 双轴平滑渐变 + 高频二值纹理 + 解析矩形/圆形图元。
- 重建帧由 8×8 正交 DCT-II → 格点量化（`step=1+(100-q)/5`）→ DCT-III 逆变换 → 四舍五入到 `uint8` 得到。
- 帧文件为 `magic + 头 + 像素负载 + CRC32`，损坏触发 `E_DECODE`。
- 真值清单 `manifest.json` 记录种子、尺寸、通道、帧数、质量档位，以及采样帧的 PSNR 区间、解析边缘像素坐标与逐块误差最近秩分位数；缺失任一字段返回 `E_SCHEMA` 并指出字段名，绝不从缺失真值推断。

## 3. 定点精度与截断可观测

- SSIM 的均值、方差、协方差、亮度/对比度项全部使用 64 位 Q16.16；乘法用 128 位中间积，除法用 128 位被除数长除，开方为整数牛顿迭代。
- 每次丢弃小数位都记录 `Truncation{location,input,lost_lsb}`，按符号位置（如 `window.ssim.final`）随块持久化，首个截断位置可复算。
- 仅最终 `log10`（PSNR）使用浮点，因为定点无等价超越函数；平方误差累加仍为整数。

## 4. 误差上界推导（仅来自已观测参数）

正交 DCT 每个系数量化误差 `|e_k| <= step/2`，基函数幅度 `|phi|^2 <= 2/N`：

```
|像素误差(取整前)| <= N^2 * (step/2) * (2/N) = N * step
像素域上界 B      = N*step + 1            （+1 为 uint8 取整）
MSE 上界           = B^2
PSNR 下界(dB)      = 20*log10(255/B)
```

逐块记录系数量化前后误差、逆变换像素误差、相邻块边界误差跃变与空间邻接路径；任一块实测像素误差超过 `B` 或实测 PSNR 低于解析下界，返回 `E_BOUND` 并附带帧号、块坐标与实测值。质量档位下降时步长单调增大、PSNR 下界与实测 PSNR 同步下降。

## 5. 边界一致性与顺序无关合并

- 全局确定性 SSIM 窗口网格（光栅序），每个窗口按其左上角像素唯一归属一个块。
- 所需单边重叠边带 `halo >= max(window-1, stride)`，不足返回 `E_BAND` 并给出所需/现有宽度。
- 帧合并时按窗口 `(topY,topX)` 排序后再做定点平均，因此结果与块完成顺序、并行度无关；测试证明分块合并与整帧单次评估的 SSIM 完全相等。

## 6. 检查点与三路径等价

- 每提交一块，在互斥锁内以单条 `Write + fsync` 追加自描述 JSON 行（含 CRC32、输入哈希、参数哈希、参数/算法版本、重建像素与摘要）。
- 重启时版本、清单哈希、质量或参数哈希任一不匹配 → 删除整个快照并全量重算，禁止混用旧块。
- 撕裂尾记录（强制 kill）在加载时截断，仅完整落盘块可恢复；崩溃注入通过 `-crash-after N` 模拟。
- `realtime`、`resume`、`rebuild` 三路径对同参数产出相同重建像素、逐块指标、区间统计与全局提交序列；`cmd/compare` 为纯函数，只读不写，首个差异定位到帧/块/字段/像素坐标，不一致返回 `E_DIVERGE`。

## 7. 并发资源边界

- 工作线程数由 `-workers` 限制；像素缓冲信号量上限为 `max-pixel-buf / 单块像素数`，获取不到缓冲时阻塞排队，不丢块。
- 并行计算与单线程串行的块内容一致，提交始终按全局光栅序串行落盘，提交序列相同。

## 8. 错误码与退出码

错误以 JSON 输出到 stderr，退出码为稳定整数：

| 码 | 退出码 | 触发场景 |
| --- | --- | --- |
| E_SCHEMA | 10 | 清单字段缺失/非法、质量档位不支持 |
| E_DECODE | 11 | 帧魔数/CRC/长度损坏 |
| E_PARA | 12 | 窗口/滑步越界、未知运行模式 |
| E_STATE | 13 | 块状态机非法迁移 |
| E_BAND | 14 | 重叠边带不足 |
| E_BOUND | 15 | 实测误差击穿解析上界 |
| E_DIVERGE | 16 | 三路径或验证复算不一致 |

## 9. 运行方式

```bash
go build ./...
bash scripts/run_acceptance.sh     # 端到端：生成+三路径+崩溃续跑+比对+独立验证
bash scripts/run_realtime.sh
bash scripts/run_resume.sh
bash scripts/run_rebuild.sh
bash scripts/run_compare.sh
bash scripts/run_verify.sh
go test ./...                       # 单元/集成测试（含 -race 验证）
```

Windows 使用 `scripts/*.cmd` 同名脚本，退出码语义一致。
