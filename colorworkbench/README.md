# 颜色空间转换编辑工作台（Color Workbench）

面向游戏贴图与互动媒体素材的本地、确定性、可增量重算的颜色空间转换工作台。
无 GPU、无外部 ICC 文件、运行期无网络依赖；仅依赖 `numpy` 与 `PySide6`。

## 环境准备（本地 pip 安装）

仓库环境无 root，使用用户态 `uv` 安装独立 Python 3.11：

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install numpy PySide6
```

> 仅安装阶段访问 PyPI；程序运行、样本生成与测试均离线。

## 目录结构

```
colorworkbench/
  cwb/                 核心库
    errors.py          统一错误码 E_VER/E_PARA/E_DEPTH/E_WP/E_DECODE/E_DIFF/...
    model.py           归一化内部颜色模型（TRC/LUT3D/ColorProfile/TransformConfig）
    profile_io.py      三套版本配置解析与校验
    trc.py             参数式/表格式 TRC 反演与编码、两类差异记录
    color_math.py      矩阵、3D LUT 三线性插值、Bradford 适配、三种渲染意图
    pipeline.py        像素批 + 五阶段缓存转换管线、差异统计、直方图、热力图
    editor.py          工作副本编辑、原子提交、回滚、依赖失效查询
    executor.py        线程池、并行度上限、缓冲配额阻塞、串行快照写入
    snapshot.py        版本化快照、哈希校验、整体作废与断点恢复
    image_io.py        本地 .cimg 容器编解码（E_DECODE/E_DEPTH）
    integrity.py       运行前清单完整性/位深/离线检查
    engine.py          门面：串联全链路、增量/全量一致性比对
    ui/main_window.py  PySide6 界面
  sampler/generator.py 固定种子样本生成器
  samples/             生成的图像、三套版本配置、清单、损坏样本
  tests/               36 个 unittest 用例
  scripts/             demo.py、run_tests.py
```

## 快速开始

```bash
# 1. 生成确定性样本
PYTHONPATH=. ../.venv/bin/python sampler/generator.py samples

# 2. 运行全部测试（离屏 Qt）
QT_QPA_PLATFORM=offscreen PYTHONPATH=. ../.venv/bin/python scripts/run_tests.py

# 3. 端到端验收演示
PYTHONPATH=. ../.venv/bin/python scripts/demo.py

# 4. 启动图形界面（有显示环境）
PYTHONPATH=. ../.venv/bin/python -m cwb.app
```

## 三套配置版本差异

| 维度 | 版本 1 legacy | 版本 2 middle | 版本 3.0 modern |
| --- | --- | --- | --- |
| 版本标签 | `profile_version` | `ver` | `schemaVersion` |
| 矩阵 | 三个 colorant 列标签组装 | 行主序 `matrix3x3` | `matrix.rows` |
| TRC | 参数 gamma | 表格采样（必为 table） | 参数 sRGB/gamma 或表格 |
| 白点 | `media_white_point` 可选 | `wp` 可选 | `whitePoint` 可选 |
| 意图枚举 | ICC 数字 0/1/3 | 短字符串 perc/rel/abs | kebab 字符串 |
| 3D LUT | 无 | 可选 | 可选 |

白点缺失时仅取配置文件“声明默认值”并写入转换痕迹，绝不内部猜测；
两者皆无返回 `E_WP`。

## 转换管线（逐像素批、五阶段）

`S1 源TRC反演 -> S2 矩阵/3D LUT 进入 PCS -> S3 白点适配+渲染意图
-> S4 PCS 逆矩阵 -> S5 目标 TRC 编码+位深量化`

- 渲染意图：相对比色（Bradford 适配）、绝对比色（不适配）、
  感知（适配 + 逐元素单调 soft-knee 收缩），共用同一解析模型。
- 8/16 位输入保持尺寸与通道数；16 位输出不会静默降为 8 位，
  位深不符返回 `E_DEPTH`。
- 表格 TRC 与参数 TRC 反演差异由 `trc.trc_inversion_difference` 量化记录。

## 增量与全量一致性

编辑仅改工作副本，提交时原子写回并按“参数→起始阶段”失效受影响批的下游缓存，
未受影响批复用缓存。`engine.verify_equivalence` 比对输出图像、逐像素差异统计
与通道直方图，首个不一致像素的坐标、所属批与阶段随 `E_DIFF` 返回。

## 快照与断点恢复

批文件经临时文件 + 原子替换、由快照锁串行写入；全部批完成后才原子提交
`manifest.json`。重启时任一算法/配置/参数版本或内容哈希不匹配，整个快照作废、
从源图像重算，禁止混用旧批。进程在 manifest 提交前被强杀时，仅视为不完整快照，
因此不保证生成完整差异报告。

## 并发与资源边界

线程池受 `max_workers` 限制；像素缓冲由全局配额信号量控制，耗尽时工作线程
阻塞而非丢批；共享快照写入串行化，批提交顺序按编号稳定。并行与串行对同一
编辑序列产出逐像素一致的输出与相同批提交序列（见 `test_concurrency.py`）。

## 错误码

| 码 | 触发场景 |
| --- | --- |
| `E_VER` | 未知配置版本号 / 必填标签缺失 |
| `E_PARA` | 标签值越界（异常携带源标签名） |
| `E_DEPTH` | 位深声明与数据不符 / 非法输出位深 |
| `E_WP` | 白点缺失且无声明默认值 / 非法意图 |
| `E_DECODE` | 图像损坏、魔数/负载/清单错误 |
| `E_DIFF` | 增量与全量结果、统计或直方图不一致 |
| `E_SNAPSHOT` | 快照版本/哈希不匹配或不完整 |
| `E_BATCH` | 单批转换内部异常（隔离，可重试） |
