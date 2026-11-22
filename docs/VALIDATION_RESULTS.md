# 实测验证结果

记录环境：WSL2 Ubuntu 26.04，Python 3.14.4，numpy 2.5.5，PyQt5 5.15.11
（cp38-abi3 wheel）。所有命令在仓库根目录的 `.venv` 内执行，无容器、
无数据库、无外部服务。

## 1. 环境与离线依赖

- `pip install` 固定版本：`numpy==2.5.5`、`PyQt5==5.15.11`。
- wheel 已缓存到 `wheelhouse/`，断网安装验证通过：
  `pip install --no-index --find-links wheelhouse numpy==2.5.5 PyQt5==5.15.11`
  输出 `offline install OK 2.5.5`。

## 2. 单元/集成测试

命令：`python -m unittest discover -s tests`

结果：`Ran 24 tests ... OK`，覆盖

- 样本生成确定性、PNG 往返、生长顺序无关、形态学恒等；
- E_DECODE / E_META / E_PARA / E_SEED / E_SPILL / E_REGION / E_BLOCK；
- 状态机合法/非法迁移（E_STATE）、提交冻结、失败块重试隔离；
- 中断恢复、版本不匹配整份作废（E_VERSION）、哈希损坏（E_HASH）；
- 独立验证器对掩码/统计篡改的首个像素定位；
- 有界并发参数越界、串行与并发掩码哈希一致、低对比度样本。

## 3. 掩码与几何真值偏差

声明面积容差 10%。三个确定性样本（256x256，2x2 块，每块一个区域）：

| 样本 | 最大面积相对偏差 | 平均面积相对偏差 | 最大重心误差(px) | 容差内 |
| --- | --- | --- | --- | --- |
| sample_01_blocks（强对比矩形） | 0.0 | 0.0 | 0.0 | 是 |
| sample_02_circles（含低对比度圆） | 0.001418 | 0.001033 | 0.0 | 是 |
| sample_03_mixed（混合形状） | 0.001886 | 0.000747 | 0.0 | 是 |

矩形样本掩码与真值逐像素一致（面积/重心/外接框偏差为 0）；圆形样本
偏差来自像素栅格化边界，仍远小于声明容差。

## 4. 确定性（逐像素一致）

同一参数两次独立运行、以及串行（1 worker）与有界并发（3 workers）
运行的全图掩码 SHA-256 完全相同：

- sample_01_blocks：`7e6383979427c25f...`（串行=并发=中断恢复后）

独立验证器对三个样本均输出 `passed=True, mismatch_count=0`。

## 5. 异常注入链路（示例 `examples/run_demo.py` 实测）

| 注入 | 错误码 | 关键上下文 |
| --- | --- | --- |
| 种子移到背景 (4,4) | E_SEED | `seed=[4,4]`, bbox 字段回传 |
| color_tol=260 / gradient_tol=400 | E_SPILL | `ratio=1.9787 > 1.35` |
| 删除清单区域 b03 | E_META | `missing=[b03]`，禁止进入分割 |
| min_region_pixels=10^9 | E_REGION | `measured_count=0, expected_count=1` |
| bad_block mode=corrupt/zero | E_BLOCK | 块 SHA-256 回传，仅该块 FAILED |
| 非法状态迁移 | E_STATE | 原状态保持 COMMITTED |
| 参数/算法/图像版本改动 | E_VERSION | 快照整份作废，全量重算 |
| 产物 npy/json 篡改 | E_HASH / 验证失败 | 首个不一致像素全局坐标、块、阶段 |

失败块重试：移除注入后 `FAILED -> RUNNING -> COMMITTED`，其他块已提交
结果不受影响。

## 6. 中断恢复

示例在提交 b00/b01 后触发取消（E_CANCELLED），以新引擎视角调用
`restore()`：仅恢复检查点中版本匹配的已提交阶段，继续处理 b02/b03，
最终掩码哈希与未中断的正常路径完全一致，独立验证通过。强制终止语义：
未完成 `os.replace` 的阶段丢失，重启后重算该阶段，不生成强制终止退出报告。

## 7. GUI 离屏渲染

`QT_QPA_PLATFORM=offscreen python scripts/render_overlay.py ...` 成功
产出 `workdir/overlay.png`，掩码叠加与四个区域边界对齐。
