# 验收记录

验收日期：2026-11-22  
工作目录：`/home/liang/feedback/uid190c93df-default-69`  
种子：`20261122`  
执行模式：本地进程、无 Docker/容器、无网络业务请求。

## 环境

| 项目 | 结果 |
|---|---|
| Python | 3.14.4 |
| NumPy | 2.5.5 |
| PySide6 | 6.12.0 |
| pytest | 9.2.0 |
| 虚拟环境 | `.venv` |
| 显示 | `DISPLAY=:0` / `WAYLAND_DISPLAY=wayland-0` |
| 磁盘剩余 | 验收时约 914 GiB |

## 自动化测试

```text
$ .venv/bin/python -m pytest -q
........................                                                 [100%]
24 passed
```

覆盖：

- 三版解析、v1/v2 缺失字段推导与来源标记；
- 未知版本、缺字段、时值与拍号矛盾的精确定位；
- v3->v2/v1 显式损失、允许损失降级、v1/v2 升 v3、v3 往返时值保持；
- 跨速度段积分、连奏/断奏、跨小节踏板；
- 扰动音符的时间误差定位、音域越界定位、最大偏差音符；
- 求解字节确定性、协作式取消；
- 不可变发布、版本/哈希陈旧错误；
- 四类增量编辑、缓存复用、未知编辑保守全量、与全量重算一致；
- 强制终止续算、哈希链断链、版本不符、字段缺失；
- 同种子全产物逐字节一致；
- GUI offscreen 冒烟。

## 样本与基线

命令：

```bash
.venv/bin/python -m score_engine demo \
  --out artifacts/acceptance --seed 20261122
```

结果：8 个乐谱文件、8 个基线表现序列、24 条轨迹记录。

| 文件 | 版本 | 音符 | 基线内容哈希 | 违规 |
|---|---:|---:|---|---:|
| `classic_v1.json` | 1 | 16 | `3f06446b32bd108071ee1ff4ad8245e20b7f4c6aedde07ff671d490876e130bd` | 0 |
| `classic_v1_upgraded_v3.json` | 3 | 16 | `7b6b593dd9918b9c626567c2e55c2533e305bbb13ea9ae7e8083d3ed52a5c1b6` | 0 |
| `expressive_v2.json` | 2 | 20 | `95e302644d212eaf2c14bf6d228cb76b4b8ec37dc420c5c709be4612635b934c` | 0 |
| `expressive_v2_upgraded_v3.json` | 3 | 20 | `290fb4697f1ae536913e995828b8d7701ea793ba54c0e2dfdf7a457e6786e173` | 0 |
| `range_violation_v3.json` | 3 | 32 | `02d0c6e09948c4bcdb6ce284dff8704168e604ac0bf2c3ec6b294c09e439280b` | 1 |
| `virtual_v3.json` | 3 | 32 | `bbeafea346051961145e1bb090837b282a9d66c2c6815c5f5a029a2965237b7b` | 0 |
| `virtual_v3_to_v1.json` | 1 | 12 | `fa1e94e73bc4547ad29a41ee85820c0d67d84357e6c7dd02d22351c4a6aaf006` | 0 |
| `virtual_v3_to_v2.json` | 2 | 32 | `2d36d51f1c498ce461fac28024da63bbd736ab830994cdcedc190bbcbe43bf5d` | 0 |

音域越界样本定位到 `P2-N003`，越界距离 8 个半音，验证报告 `violations` 中标记 `pitch_out_of_range`。

## 版本转换

- v1 升 v3：`loss_count=0`。
- v2 升 v3：`loss_count=0`。
- v3 降 v2：`loss_count=128`，字段 `expression`、`part.grouping`、`pitch_range`、`tempo_segments`。
- v3 降 v1：`loss_count=256`，另含 `velocity`、`articulation`、`pedals`、`time_signatures`。

全部损失逐音符写入 `manifests/conversion-report.json`，未静默丢弃。

## 增量重解

轨迹：`artifacts/acceptance/trace.jsonl`

| 编辑 | 受影响 | 复用 | 内容哈希前缀 |
|---|---:|---:|---|
| 表情锚点 | 24 | 8 | `e51588ed7c253185` |
| 速度 BPM | 9 | 23 | `164b09c964fd6b67` |
| 踏板 | 11 | 21 | `d2266d5041f94278` |
| 单音符力度 | 1 | 31 | `9681014356e1a04b` |

最终增量序列与对应全量重算：

```text
incremental_hash = 9681014356e1a04b4758291670dcaa740f1bab3cb1b4bab2098fc8d2a9fd8403
fresh_hash       = 9681014356e1a04b4758291670dcaa740f1bab3cb1b4bab2098fc8d2a9fd8403
equivalent       = true
```

## 强制终止恢复

第一次执行在第 2 个声部提交后模拟强制终止。第二次执行：

- 从快照恢复 `P1`、`P2`，共 20 个音符；
- 只新解 `P3` 的 12 个音符；
- 已恢复与新解音符无重复；
- 结果与全量重算逐字节一致；
- 最终内容哈希 `bbeafea346051961145e1bb090837b282a9d66c2c6815c5f5a029a2965237b7b`；
- 链尾 `678fc2e582399e89de0cb6cbd315fa629d94d8c06b2a75f1316c8f52c438c9d1`。

断链、字段缺失和快照版本不符均有测试验证返回 `E_RECOVERY_INVALID` 并定位首个断点。

## GUI

```text
$ QT_QPA_PLATFORM=offscreen .venv/bin/python -m score_engine gui \
    --score artifacts/acceptance/scores/virtual_v3.json --smoke
exit code: 0
```

已验证四类画布可构建、求解结果可渲染、异步线程可结束。真实桌面运行可使用同一命令去掉 `--smoke`。

验收脚本同时保存四张实际渲染截图：

- `artifacts/acceptance/gui-tab-1.png`：乐谱网格与偏差热力；
- `artifacts/acceptance/gui-tab-2.png`：表情与速度曲线；
- `artifacts/acceptance/gui-tab-3.png`：踏板区间；
- `artifacts/acceptance/gui-tab-4.png`：版本时间轴。

## 产物

- `artifacts/acceptance/scores/`
- `artifacts/acceptance/baselines/`
- `artifacts/acceptance/recovery/`
- `artifacts/acceptance/manifests/`
- `artifacts/acceptance/trace.jsonl`
- `artifacts/acceptance/acceptance.log`（运行验收脚本生成）
- `artifacts/acceptance/gui-tab-{1,2,3,4}.png`

最终校验摘要：

```text
a3b11895949172cd246d46ccf1fabfcbcb3017388c0b5255b2c0964359c781e4  trace.jsonl
5af641f7242b3e3ef30089c6fdf9d78fb60bc5932bdc9213a448f88c91206088  baseline-manifest.json
a0c5e426203f6450647e06f6c8ed0b2010800a13a81796dcb7f27f46cb2a1a06  conversion-report.json
```

`version-loss-report.json` 为 `E_VERSION_LOSS`、`detail_count=128`，首个位置
`note_id=P1-N001`、`field=expression`。

