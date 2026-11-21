# Validation Record

执行日期：2026-11-22  
环境：Python 3.11.18、NumPy 2.4.6、pytest 9.2.0

## 命令

```bash
.venv/bin/python -m lightprobe generate-samples --out samples
.venv/bin/python -m lightprobe verify --report artifacts/reports/verification.json
.venv/bin/python -m pytest
```

## 两级场景结果

| 场景 | 探针数 | 自动策略 | 完整采样 | 插值探针 | 基线绝对 RMS | 基线相对 RMS |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| street_corner_room | 75 | dense | 75 | 0 | 0.0550 | 2.83% |
| city_district | 1734 | sparse | 243 | 1491 | 0.3116 | 19.09% |

策略审计在同一验证探针、同一 8 法线和同一 192 方向真值上比较：

| 场景 | dense RMS | sparse RMS | 选定误差包络 |
| --- | ---: | ---: | ---: |
| street_corner_room | 0.0478 | 0.3665 | 0.2180 |
| city_district | 0.0781 | 0.3296 | 1.8130 |

## 增量复用

| 场景 | 变化 | 种子 | 影响闭包 | 重烘 | 复用 | 最大系数偏差 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| street_corner_room | 移动 street_lamp | 7 | 7 | 7 | 68 | 0.9629 |
| city_district | 移动 tree_c | 115 | 911 | 911 | 823 | 1.2695 |

## 快照恢复

| 场景 | 分块数 | 已提交探针 | 恢复重烘 | 恢复复用 | 内容哈希一致 |
| --- | ---: | ---: | ---: | ---: | --- |
| street_corner_room | 3 | 75 | 0 | 75 | 是 |
| city_district | 55 | 1734 | 0 | 1734 | 是 |

另有测试在独立子进程提交 2 个分块后调用 `os._exit(137)` 模拟强制终止。恢复进程
仅从磁盘读取 32 枚已提交探针，重烘其余 43 枚；最终内容哈希与不中断烘焙一致。

## 测试

```text
33 passed in 8.90s
```

完整验收 JSON：`artifacts/reports/verification.json`。
