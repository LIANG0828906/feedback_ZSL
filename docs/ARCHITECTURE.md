# 图像分割与掩码处理工作台 — 架构与技术说明

## 1. 范围与约束

- 纯本地进程运行，不使用 Docker/容器、数据库或外部服务。
- 运行期只依赖 `numpy`；可选 GUI 依赖 `PyQt5`。PNG 编解码为仓库内置的
  纯 Python 实现（`segworkbench/utils/pngcodec.py`），无需 Pillow。
- 所有随机性来自显式种子（`numpy.random.Generator(seed)`），同种子复算
  逐字节一致。

## 2. 模块分层

| 层 | 模块 | 职责 |
| --- | --- | --- |
| 基础 | `core/errors.py`, `core/versioning.py`, `utils/hashing.py` | 稳定错误码、算法/参数版本、SHA-256 摘要 |
| 样本 | `pipeline/samplegen.py`, `pipeline/loader.py` | 确定性样本生成、加载校验、观测基线登记 |
| 算法 | `pipeline/growing.py`, `morphology.py`, `postprocess.py`, `geometry.py` | 区域生长、形态学、连通域、几何与轮廓 |
| 编排 | `pipeline/states.py`, `blocks.py`, `engine.py`, `concurrency.py`, `assemble.py` | 状态机、分块、三阶段提交、有界并发、拼接 |
| 持久化 | `persistence/checkpoint.py` | 原子串行检查点、版本指纹、快照作废 |
| 验证 | `validation/verifier.py` | 独立重算与逐项比对（不导入引擎） |
| 界面 | `gui/viewer.py` | 可选 PyQt5 查看器（支持 offscreen） |

## 3. 确定性设计

1. **区域生长**：固定 4-邻域 BFS，访问集合只做标记，输出掩码与遍历顺序
   无关；颜色距离与边梯度阈值均为纯函数。
2. **连通域/形态学**：按行扫描 BFS 分配标签；结构元素固定为实心方块。
3. **坐标约定**：`x=列、y=行`，原点左上角，像素中心为整数坐标；块内统计
   在 stats 阶段统一换算为图像全局坐标。重心取掩码像素一阶矩，不经过轮廓
   近似，因此与轮廓阶数无关。
4. **并发**：线程池只并行计算；所有检查点写入在 `CheckpointStore` 的
   线程锁 + 文件锁内串行，块按编号、阶段按 `grow/clean/stats` 固定顺序。

## 4. 状态机与提交边界

每个块的每个阶段独立维护四态：

```
PENDING -> RUNNING -> COMMITTED
RUNNING -> FAILED；PENDING -> FAILED；FAILED -> RUNNING（重试）
```

非法迁移抛 `E_STATE` 且原状态不变。已提交阶段在引擎重建后直接从检查点
读取，参数调整只作用于未提交块/阶段，已提交掩码不会被静默改写（测试
`test_parameter_change_does_not_rewrite_committed` 覆盖）。

## 5. 检查点与恢复

```
<root>/snapshot.json              快照头（版本、图像/真值指纹、块阶段索引）
<root>/blocks/bXX/grow.npy|json   生长掩码 + 元数据哈希
<root>/blocks/bXX/clean.npy|json  最终掩码 + 元数据哈希
<root>/blocks/bXX/stats.json      几何统计与真值偏差
<root>/snapshot.lock              跨线程/跨进程互斥
<root>/tombstone.log              快照作废追溯
```

- 写入：临时文件 + `fsync` + `os.replace` 原子替换，锁内串行，不交错。
- 恢复：校验图像哈希、参数版本、算法版本、快照格式版本；任一不匹配则
  删除全部块产物与快照头（禁止部分混用旧块），抛 `E_VERSION` 后全量重算。
- 强制终止：只可能丢失尚未 `os.replace` 的当前阶段；重启后仅恢复已提交
  阶段，不承诺强制终止时生成完整退出报告。

## 6. 错误码链路

| 错误码 | 触发方式（输入/参数注入，不改源码） | 失败阶段 |
| --- | --- | --- |
| `E_DECODE` | 清单缺失 / PNG 字节或 CRC 损坏 | 加载 |
| `E_META` | 通道、尺寸、块网格、区域数量不一致（带字段名） | 加载 |
| `E_PARA` | 颜色/梯度容差、结构元素半径、种子越界 | 参数校验 |
| `E_SEED` | `seed_points` 注入到背景 bbox 之外 | grow |
| `E_SPILL` | 大颜色/梯度容差导致生长面积超比例阈值 | grow |
| `E_REGION` | 真值区域缺失或块内连通域数量异常 | 加载/stats |
| `E_BLOCK` | `injections.bad_block` 注入全零/损坏块 | grow |
| `E_STATE` | 非法状态迁移 | 状态机 |
| `E_VERSION` | 图像哈希/参数/算法/格式版本不匹配 | 恢复 |
| `E_HASH` | 产物数组或元数据哈希损坏 | 加载/验证 |
| `E_CANCELLED` | 协作式取消（阶段边界生效） | 编排 |

单块异常只把该块当前阶段置 `FAILED`，不污染其他块；去掉注入后可重试。
所有注入点通过 `utils/logging_setup` 的 WARNING/ERROR 日志追溯。

## 7. 独立验证器

`validation/verifier.py` 不导入引擎与后处理实现，仅读取持久化产物与
清单真值：

1. 复算图像哈希、真值指纹并核对快照版本；
2. 校验每个阶段数组/元数据哈希与快照索引；
3. 用验证器自带的内联形态学从持久化 `grow` 掩码独立重算 `clean` 掩码，
   出现差异时返回首个不一致像素的全局坐标、块编号、阶段；
4. 独立重算连通域几何并与 stats 记录、解析真值逐项比对；
5. 拼接全图掩码并输出稳定哈希。

## 8. Windows / WSL 环境说明

- 目标环境为 Windows + Python 3.10+。本仓库在 WSL2 Ubuntu 26.04 +
  Python 3.14 下实测；PyQt5 使用 `cp38-abi3` 稳定 ABI wheel，3.10–3.14
  均可安装。
- 离线安装：`pip install --no-index --find-links wheelhouse -r requirements-gui.txt`。
- Windows 上文件锁自动退化为 `msvcrt.locking`，Linux/WSL 使用 `fcntl.flock`。
- 无头环境运行 GUI 渲染：设置 `QT_QPA_PLATFORM=offscreen`。
