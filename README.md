# a11y_media — 游戏过场与互动媒体无障碍呈现领域组件

以统一定点时间单位(采样点数)贯穿
**包络检测 → 台词窗口推导 → 字符高亮排程 → 渲染与朗读输出** 全链路,
每个阶段输出可观测的误差统计与上界估计; 支持长工程分段推进、
可暂停 / 断点恢复, 并提供纯函数式查询接口与本地示例。

## 环境

- Python 3.11+(本仓库实际验证于 Python 3.14)+ NumPy + PySide6, 见 `requirements.txt`
- 不使用 Docker; 样本由固定种子本地确定性生成, 不依赖网络与外部素材

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
```

## 目录结构

```
a11y_media/            # 领域组件包
  errors.py            # 错误码: E_SCHEMA / E_PARA / E_ERR / E_STATE / E_DIVERGE
  timebase.py          # 统一整型时间单位(采样点)与版本常量
  envelope.py          # 阶段1: 逐帧能量、语音段检测、hop 网格边界量化
  windows.py           # 阶段2: 台词窗口推导(整型有理分配, 最大余数法)
  schedule.py          # 阶段3: 逐字符高亮排程与时长归一化
  errprop.py           # 阶段4: 误差传播报告与累积上界(E_ERR)
  loader.py            # 工程加载与结构校验(E_SCHEMA / E_PARA)
  generator.py         # 确定性样本与真值清单生成器
  statemachine.py      # 分段状态机(E_STATE)
  checkpoint.py        # 原子串行检查点, 哈希/版本不匹配整体作废
  engine.py            # 实时 / 断点恢复 / 全量重算三条处理路径
  compare.py           # 三路径一致性比对(E_DIVERGE)
  api.py               # 纯函数式查询接口
scripts/               # 样本生成 / 损坏样本 / 三路径比对 / 验收总入口
examples/              # CLI 示例与 PySide6 桌面示例
tests/                 # pytest 测试(41 项)
```

## 快速开始

```bash
# 1. 生成确定性样本(4 条工程, 种子 42)
.venv/bin/python scripts/generate_samples.py --count 4 --seed 42

# 2. 三路径一致性比对(实时 / 断点恢复 / 全量重算)
.venv/bin/python scripts/compare_paths.py data/samples/proj_000

# 3. 生成损坏样本副本并验证异常码
.venv/bin/python scripts/make_corrupt_samples.py

# 4. CLI 完整示例(含误差报告导出)
.venv/bin/python examples/demo_cli.py data/samples/proj_000 --export data/report.json

# 5. 桌面示例(无显卡环境)
QT_QPA_PLATFORM=offscreen .venv/bin/python examples/demo_gui.py data/samples/proj_000 --smoke

# 6. 一键验收
.venv/bin/python scripts/validate_all.py
```

## 关键设计

**统一时间基**: 全链路只使用整型采样点; 与微秒的换算集中在
`timebase.py`(纯整型有理运算), 时间基版本 `TIME_UNIT_VERSION` 与
`ALGO_VERSION` 写入每个检查点, 不匹配即整体作废快照。

**误差上界推导**(仅由帧长 / 帧移 / 量化步长推导, 不从真值反推):

| 阶段 | 误差项 | 累积上界 |
| --- | --- | --- |
| 包络检测 | 边界量化误差 | `hop // 2` |
| 窗口推导 | 累积偏差 | `hop // 2`(同单位无新增量化) |
| 字符排程 | 取整残差 | `hop // 2 + 1` |
| 整工程 | 总朗读时长偏差 | `2 × 句数 × (hop // 2) + 1` |

检测边界吸附到最近 hop 整数倍, 因此帧移按整数倍收紧时边界偏差单调不增;
实测超界抛 `E_ERR` 并指出句号与误差项名称。

**字符排程**: 权重为正整数, 归一化为整型有理运算(分母 = 权重和),
余数按最大余数法确定性分配(同余取下标小者), 逐字符区间首尾相接、
总时长恰等于台词窗口。

**分段状态机**: `PENDING → PROCESSING → COMMITTED / FAILED`,`FAILED → PROCESSING`
(重试); 非法迁移抛 `E_STATE` 且状态不变。已提交分段结果冻结,
语速 / 权重调整只作用于未提交分段。

**检查点**: 每个已提交分段经 "临时文件 + fsync + os.replace" 原子写入,
全局写锁保证串行不交错; 重启时内容哈希或任一版本不匹配即整体作废,
禁止部分混用旧分段。

**并行与资源边界**: `ThreadPoolExecutor` 限制最大并行度(`max_workers`)
与在途缓冲(`buffer_limit`); 单段异常仅置该段 `FAILED`, 可重试, 不污染其他段。

## 组件接口

```python
from a11y_media.api import MediaAccessibilityService

svc = MediaAccessibilityService("data/samples/proj_000", checkpoint_root="data/cp")
svc.run_realtime()                      # 或 run_full() / Engine 断点恢复
svc.get_window(0)                       # 取某句台词窗口 (start, end)
svc.get_char_interval(0, 3)             # 取某字符高亮区间 (start, end)
svc.get_error_report()                  # 整工程误差报告(深拷贝)
```

查询只读不可变结果快照, 不影响内部状态。

## 实际验证结果

验证环境: Python 3.14.4 / NumPy 2.5.5 / PySide6 6.12.0 / pytest 9.2.0。

- `pytest tests/ -q`: **41 passed**
  (加载校验 11、包络与窗口 4、字符排程 7、误差传播 5、状态机与检查点 6、
  三路径一致 3、组件接口 5)
- 4 条样本工程(种子 42~45)三路径比对全部 `OK: 三路径一致`,
  总朗读时长偏差均为 0, 远低于上界(如 proj_000: 0 ≤ 961)
- 8 类损坏样本副本异常码全部符合预期:
  `missing_field/bad_segment_ref/window_out_of_segment/bad_total_samples → E_SCHEMA`,
  `bad_speed/nan_speed/zero_weights/bad_hop → E_PARA`(均带字段名)
- 帧移收紧单调性: hop 逐次减半(240→120→…), 段边界总偏差单调不增
- 检查点: 崩溃后快照仅含已提交段; 版本号 +1 后快照被整体作废并全量重算
- GUI 离屏自检(`--smoke`)退出码 0
