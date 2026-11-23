# LoudBench

面向游戏与互动媒体音频素材的广播响度与动态范围处理工作台。
K 加权门限响度（BS.1770-4）、动态压缩、前瞻限幅、全局响度归一、
线程池并行调度、原子检查点/断点恢复与 PySide6 可视化，全部本地运行。

## 快速开始

```bash
# 环境（仓库实际使用 uv 安装的 CPython 3.11.18）
python3.11 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt

# 一键验收：生成确定性样本 -> 三路径一致性 -> 全量测试
bash run_checks.sh
```

## 常用入口

```bash
export PYTHONPATH=src
.venv/bin/python samples/generate_samples.py --out samples/data
.venv/bin/python -m loudbench.cli.main verify      # 并行/串行/恢复三路径
.venv/bin/python -m loudbench.cli.main run --serial
.venv/bin/python -m loudbench.cli.main run --restore
.venv/bin/python -m loudbench.gui.app              # 图形界面
.venv/bin/python -m unittest discover -s tests     # 44 个测试
```

## 错误码

| 码 | 含义 |
|---|---|
| `E_PARA` | 参数非法（采样率/帧参数/压缩限幅/缓冲预算越界），带字段名 |
| `E_STATE` | 状态机非法迁移或归一提交乱序，原状态保持 |
| `E_DECODE` | 样本/清单损坏或内容哈希不匹配 |
| `E_DIFF` | 路径间结果不一致，定位段号与首个不一致采样 |
| `E_CANCEL` | 用户取消 |

详细架构、偏差说明与实际验证结果见 `docs/TECHNICAL_NOTES.md`。
