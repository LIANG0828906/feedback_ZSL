# 测试运行记录

## 环境

```text
Python 3.11.18
NumPy 2.4.6
OpenCV 5.1.0
PyQt5 5.15.11
pytest 9.2.0
```

## pytest

```text
$ .venv/bin/python -m pytest -q
.................................                                        [100%]
33 passed in 1.68s
```

覆盖 33 项：

- 参数范围/阈值倒置/边带不足/缓冲上限；
- 单通道、三通道、单通道轴与阶段尺寸；
- 短片段剪枝；
- 分块五阶段与整图五阶段逐像素一致；
- 四态合法/非法迁移与未提交回滚；
- 单块失败隔离与重试；
- 优雅停止、原子恢复、内容哈希与参数版本失效；
- 三路径掩膜/阶段/统计/提交序列比较；
- `E_DIFF` 首像素、分块、阶段定位；
- 子进程 `os._exit(17)` 强制终止后从 7 个提交恢复；
- offscreen PyQt5 窗口构建与状态矩阵。

## 完整验收

```text
$ .venv/bin/edgepipe verify \
    --manifest samples/index.json \
    --work-dir artifacts/validation_work \
    --report reports/validation_report.json \
    --markdown docs/VALIDATION.md

all_pass: true
error codes: E_PARA, E_BAND, E_STATE, E_DECODE, E_DIFF
```

4 个样本的三路径输出逐像素一致；五个阶段的分块输出与整图单块参考逐像素一致；检查点内容哈希/参数版本/算法版本/输入哈希失效均被整体检出。详细数据见 `docs/VALIDATION.md`。

