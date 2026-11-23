# CAD 几何与曲面专业桌面渲染应用

从零实现的 NURBS 曲面 CAD 渲染应用：确定性样本生成、参数曲面细分与法线、
预览/精确双路径渲染、分块并行串行提交、版本快照、断点恢复、剖面测量与
PyQt5 桌面界面。渲染核心为**纯 NumPy 确定性软件光栅器**，保证并行与串行
逐像素一致；OpenGL FBO 仅做能力探测/可选加速，驱动不支持时回退并记录
`E_MEM`。

## 安装与运行

```bash
uv venv .venv
uv pip install --python .venv/bin/python -r requirements.txt

# 离屏可重复演示（生成样本→全链路→导出帧与报告）
QT_QPA_PLATFORM=offscreen PYTHONPATH=src \
  .venv/bin/python -m cadrender.demo --workdir workdir \
  --checkpoint checkpoint --export demo_output --size 256 --workers 3

# 测试
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest tests/

# 桌面 GUI（有显示环境）
PYTHONPATH=src .venv/bin/python -m cadrender.gui
```

## 模块结构（`src/cadrender/`）

| 模块 | 职责 | 对应要求 |
| --- | --- | --- |
| `errors.py` | 错误码 E_GEOM/E_META/E_PARA/E_DIVERGE/E_VER/E_STATE/E_MEM/E_TILE 与错误链路 | 8 |
| `nurbs.py` | Cox–de Boor 基函数、有理曲面/曲线求值、解析偏导与法线 | 1,2 |
| `samplegen.py` | 固定种子样本、控制点/节点/权重/裁剪/解析真值/版本元数据 | 准备 |
| `loader.py` | 多片全量加载、节点/权重/裁剪校验、E_GEOM/E_META、裁剪内外判定 | 1 |
| `tessellation.py` | 参数域三角化、曲率联动密度、确定性法线、E_PARA | 2 |
| `camera.py` | 统一相机内外参、透视投影、近远裁剪、坐标约定 | 3 |
| `versioning.py` | 不可变版本快照、材质、冻结分块、E_VER/E_STATE | 5 |
| `renderer.py` | 软件光栅器、预览(simple)/精确(full)双路径、缓冲预算 E_MEM | 3 |
| `compare.py` | 覆盖率/包围盒/明暗分层比对、首个不一致像素与分块、E_DIVERGE | 4 |
| `scheduler.py` | 受限线程/像素预算、阻塞排队、串行稳定提交、块失败重试 | 6,8 |
| `checkpoint.py` | 原子串行持久化、内容/算法哈希校验、崩溃恢复边界 | 7 |
| `section.py` | 剖面平面求交、双端点世界/参数双读数、版本冻结 | 8 |
| `glcontext.py` | OpenGL 上下文/FBO 探测与软件回退 E_MEM 记录 | 准备,8 |
| `pipeline.py` | 应用核心编排 | 全部 |
| `gui.py` | PyQt5 界面：帧对照、分块矩阵、延迟分位、热力图、导出 | 8 |
| `demo.py` | 离屏端到端可重复演示 | 验收 |

## 关键设计决策

- **确定性优先**：所有几何与像素计算无随机量、无原子写竞争；分块按
  `tile_id` 升序串行提交，并行/串行对同版本场景逐像素一致（测试固化）。
- **双路径同参**：预览与精确共用同一 `Camera`（内外参、裁剪平面、坐标
  约定），仅 LOD 与着色模式不同；帧对象记录 LOD、着色模式与版本号。
- **版本不可变**：编辑控制点/材质即生成新版本快照，精确任务启动锁定版本；
  已提交分块在旧版本冻结，编辑只作用于新版本。
- **恢复边界**：临时文件 + 原子 `os.replace`，崩溃时未提交块不存在；
  内容哈希或算法版本不匹配则整体作废全量重算，绝不虚报完整报告。
- **预算与隔离**：信号量 + 在途像素预算实现阻塞排队并记录原因；单块异常
  标记失败可重试，不污染其他块。

实测结果见 `docs_verification.md`。
