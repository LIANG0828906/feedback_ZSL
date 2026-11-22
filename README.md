# volview — 医学影像阅片容积数据显示组件

面向医学影像阅片场景的容积（体数据）显示领域组件与完整可运行示例。
纯本地实现：Python 3.11 + NumPy，不依赖 Docker / 网络 / 外部服务，
平面图像通过离屏缓冲渲染（PGM/PNG 导出）。

## 环境准备

```bash
uv python install 3.11                 # 或任意 Python 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python numpy pytest
```

## 快速开始

```bash
# 生成确定性样本（固定种子 20240517，产出 vol_a / vol_b / vol_c）
.venv/bin/python -m volview.generator data/samples

# 端到端演示：会话 -> 标注 -> 快照 -> 跨会话复现 -> 异常注入 -> 独立验证
.venv/bin/python scripts/run_demo.py

# 全部测试（47 项）
.venv/bin/python -m pytest tests/ -q

# 独立验证工具：仅凭快照 + 体数据清单重算并逐项比对
.venv/bin/python -m volview.verify --snapshots data/snapshots --samples data/samples

# 异常注入（五类）
.venv/bin/python -m volview.inject missing-intercept
```

## 目录结构

```
volview/
  errors.py      统一错误码（E_AXIS/E_CAL/E_WIN/E_THICK/E_RANGE/E_DECODE/E_ACC）
  model.py       体数据加载、清单校验、几何模型（全有或全无加载）
  generator.py   固定种子确定性体数据生成器 + 解析几何真值
  resample.py    轴位/冠状/矢状三平面重采样 + 窗宽窗位映射
  slab.py        层厚等效（加权平均、权重归一、E_THICK 范围）
  measure.py     测距标注、世界/体素双读数、精度上界推导
  session.py     阅片会话：参数、渲染、标注、快照提交
  snapshot.py    快照存储：原子写入、串行化、保序提交、整体作废
  scheduler.py   有界线程池：最大并行度、像素缓冲上限、失败重试
  inject.py      五类异常注入 CLI
  verify.py      独立可复现验证 CLI
scripts/run_demo.py   端到端示例
tests/                47 项验收测试
```

## 坐标与平面约定

- 世界轴 `X/Y/Z`；清单 `axis_order` 声明数组维与世界轴的映射（如 `ZYX`、`XZY`）。
- 平面法向：轴位→Z，冠状→Y，矢状→X；面内（行，列）固定为
  轴位 (Y,X)、冠状 (Z,X)、矢状 (Z,Y)。
- 采样位置始终取面内轴的体素中心世界坐标，插值阶数 0/1 切换后平面尺寸
  与采样位置保持同一约定（整层位置上两阶结果逐像素一致）。
- 体素 `(i0,i1,i2)` 的世界坐标：`w[axis] = i_dim(axis) * spacing[axis]`。

## 关键设计

**错误链路**：所有异常路径抛出 `VolViewError(code, field, ...)`，
`field` 定位到清单字段、参数名或 `annotation[i].endpoint[j]`；
加载失败不留下半初始化模型（注册表只缓存成功加载）。

**层厚等效**：层厚 `t>0` 时，对中心落在 `[p-t/2, p+t/2]` 内的体素中心层
做三角加权平均并归一化；窗口在体数据边界处截断并重新归一（不丢边、不补零）；
`t` 超过轴全范围返回 `E_THICK` 并携带可用范围 `[0, (n-1)*spacing]`。

**精度上界**：端点按插值阶数吸附到采样格（0 阶→体素中心，1 阶→半体素格），
上界 `bound = sqrt(3) * max_spacing * (1 或 0.5)` 仅由间距与插值阶数推导，
不读取真值；实测偏差越界返回 `E_ACC` 并给出标注编号、实测值与上界。

**快照**：每次提交把体数据标识 + 内容哈希（sha256(payload+canonical manifest)）、
窗宽窗位、平面与位置、层厚、插值阶数、全部标注双读数、平面图像（.npy + sha256）
整体原子写入（临时文件 + `os.replace`），写锁串行化、序号条件变量保证会话内
操作顺序；重开时内容哈希 / 结构版本 / 插值阶数任一不匹配即整体作废，
按默认会话重建，禁止部分沿用。

**并发**：`PlaneScheduler` 限制最大并行度与全局在途像素预算（信号量），
预算耗尽时提交方阻塞排队而非丢任务；单任务像素上限触发行分块渲染
（结果与不分块逐像素一致）；任务失败置 `FAILED` 可 `retry()`，
不影响其他任务与已提交快照。

## 实际验证结果（本机，Python 3.11.18 + NumPy 2.4.6）

- `pytest tests/ -q`：**47 passed**（覆盖验收全部八项业务要求）。
- 跨会话复现：`restored=True pixel-identical=True readings-identical=True`。
- 测量精度：球体直径实测 20.000 mm，真值 20.000 mm，偏差 0.000 ≤ 上界 2.165 mm。
- 五类异常注入均落入预期链路：
  `missing-intercept→E_CAL(intercept)`、`window-width-zero→E_WIN(window_width)`、
  `thickness-overflow→E_THICK(thickness)`、`annotation-oob→E_RANGE(annotation[0].endpoint[1])`、
  `truncated-volume→E_DECODE(volume.npy)`。
- 独立验证工具：`{"ok": true, "verified": N}`，退出码 0。
- 并发：4 线程与单线程跑同一操作序列，平面图像逐像素一致、快照记录顺序一致。
- 层厚 0→40 mm 递增时平面高频能量单调不增（943229 → … → 560906）。

## 备注

- 任务书要求 Python 3.11；本机系统仅有 Python 3.14，故用 `uv` 安装了
  CPython 3.11.18 独立运行时于 `.venv/`，组件本身兼容 3.11+。
- 渲染输出为离屏缓冲：`scripts/run_demo.py` 导出 `data/renders/plane_*.pgm`。
