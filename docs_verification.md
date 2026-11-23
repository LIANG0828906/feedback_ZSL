# 验证记录（实际运行结果）

运行环境：Python 3.14.4 / NumPy 2.5.5 / PyQt5 5.15.11 / PyOpenGL 3.1.10 /
Pillow 13 / pytest 9.2，平台 Linux x86_64，`QT_QPA_PLATFORM=offscreen`。
OpenGL 离屏上下文由 Mesa **llvmpipe（软件实现）** 提供，OpenGL 4.5，FBO
完整性检查通过；GPU 不可用时自动回退自研 numpy 软件光栅器并在 `E_MEM`
链路记录。

## 自动化测试

命令：`QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest tests/`

结果：**49 passed**，覆盖 8 项业务要求与全部错误码路径。

| 测试文件 | 覆盖要求 | 用例数 |
| --- | --- | --- |
| `tests/test_loader.py` | 要求 1 加载/校验 E_GEOM、E_META、裁剪内外判定 | 8 |
| `tests/test_tessellation.py` | 要求 2 细分、法线、曲率联动、确定性、E_PARA | 11 |
| `tests/test_render_compare.py` | 要求 3/4 双路径、同相机、E_DIVERGE、缺数据拒绝 | 7 |
| `tests/test_versioning.py` | 要求 5 不可变快照、冻结 E_VER、非法迁移 E_STATE | 6 |
| `tests/test_scheduler.py` | 要求 6 并行/串行逐像素一致、预算排队、块失败重试 | 5 |
| `tests/test_checkpoint.py` | 要求 7 原子写、哈希/算法版本作废、崩溃恢复边界 | 5 |
| `tests/test_section.py` | 要求 8 剖面交线、世界/参数双读数、版本冻结 | 3 |
| `tests/test_gl_and_mem.py` | 要求 8 GL 探测回退、E_MEM、界面逻辑 | 3 |
| `tests/test_subdivision.py` | 细分控制网 Catmull-Clark 极限点真值 | 1 |

## 端到端演示（`python -m cadrender.demo`）

关键实测（256×256，3 工作线程，32px 分块，共 64 块）：

- 样本生成：固定种子 `20261123`，4 个 NURBS 曲面片 + 2 个细分控制网，
  内容哈希写入 `manifest.json`。
- 几何真值：4 个曲面采样点各轴最大绝对偏差均为 `0.0`（容差 1e-6）。
- 细分确定性：同密度两次细分 SHA256 签名一致；密度 8/16/32/64 三角面数
  单调不减（如 96 → 378 → 1506 → 6031）。
- 预览帧：LOD=16、`simple` 着色（仅环境光+漫反射）；精确帧：LOD 自适应、
  `full` Blinn-Phong 光照，记录版本号。
- 并行 vs 单线程串行：`pixel_identical=true`、`depth_identical=true`、
  `same_commit_sequence=true`，提交序列严格 `[0..63]`。
- 自动比对：正常基线 `passed=true`；人为大改控制点后三项指标超差，
  返回 `E_DIVERGE` 并给出覆盖率/包围盒/明暗分层偏差、首个不一致像素与分块。
- 断点恢复：模拟提交一半分块后强制终止，重启后仅恢复已持久化的 32 块、
  `complete=false`，续跑后补齐为完整帧，恢复块编号与崩溃前一致。
- 版本冻结：旧版本 36/64 块冻结，编辑生成新版本号；按不存在版本查询抛
  `E_VER`。
- 剖面测量：输出双端点世界坐标 + 参数域 (u,v) 双读数与距离，随版本冻结。

产物：`demo_output/preview.png`、`precise_parallel.png`、
`diff_heatmap.png`、`demo_report.json`；检查点
`checkpoint/tiles/v*_t*_{color,depth}.npy` 与原子 `index.json`。
