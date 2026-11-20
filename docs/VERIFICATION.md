# 验证报告

## 1. 环境

* Python 3.14.4（满足 >=3.10 要求）
* NumPy 2.5.5、PyQt6 6.12.1（含 PyQt6-Qt6 6.12.0）
* 无容器、无外部账号；样本全部由 `retarget_pipeline.samples` 本地确定性生成

## 2. 自动化测试

执行：`python -m unittest discover -s tests`

结果：**32 个用例全部通过（OK）**，覆盖 8 项业务要求：

| 测试文件 | 覆盖要求 | 用例要点 |
| --- | --- | --- |
| `test_codec.py` | 1 | 三版本往返、未知版本/缺字段/成环/退化四元数定位、拒绝部分加载、原子写无残留 |
| `test_mapping.py` | 2 | 别名优先、确定性哈希、`E_MAP_CONFLICT`、最近祖先回退 |
| `test_compat.py` | 3 | 首帧推导绑定姿态与 provenance、原生 v3 标注、`E_VERSION_LOSS` 字段清单 |
| `test_retarget.py` | 4/5 | 三版本全帧求解、跨版本统计一致、缩放仅作用一次、`E_POSE_UNRESOLVED` |
| `test_snapshot.py` | 6 | 哈希链恢复、断链 `E_RECOVERY_INVALID`、映射/版本变更触发重算 |
| `test_engine.py` | 7 | 后台完成、重复提交 `E_BUSY`、协作式取消保留快照 |
| `test_unresolved_skip.py` | 4 | 不可求解帧保留内存但跳过落盘 |
| `test_determinism.py` | 8 | 样本/日志逐字节一致、近重合旋转、GUI offscreen 构建 |

## 3. 端到端验收

执行：`python scripts/run_demo.py --workspace outputs/demo --gui`

样本库：small(6 关节/8 帧)、medium(12/24)、large(17/48)，各含 v1/v2/v3。

### 3.1 映射命中

| 样本 | 命中/总数 | 冲突 | 祖先回退 |
| --- | --- | --- | --- |
| small | 6/6 | 0 | 0 |
| medium | 12/12 | 0 | 0 |
| large | 17/17 | 0 | 0 |

### 3.2 跨版本偏差（容差 1e-6 rad）

全部版本、全部样本 `passed=true`，最大偏差在 1e-8 rad 量级（浮点误差），
远低于容差；large v3 实测最大偏差约 2.41e-6 度，FK 自洽最大约 2.96e-6 度，
逐关节逐帧单元格 816 个，无超差。

### 3.3 快照与恢复

每个样本×版本独立工程目录，快照数等于帧数（small 8、medium 24、large 48），
`last_committed_frame` 连续到尾帧；演示脚本在恢复后断言快照链完整、
映射哈希与版本一致且无需重算。

### 3.4 降级损失

v3→v1 降级报告字段：`aliases`、`bind_matrix`、`parent`、`scale`、
`translations`，并列出每个字段的受影响关节，未静默丢弃缩放/绑定姿态。

### 3.5 轨迹日志

`outputs/demo/reports/trajectory_log.jsonl` 共 240 条记录，字段含
帧号、版本、映射命中数、逐帧最大偏差、是否超差、内容哈希。

## 4. 确定性复核

两次独立运行 `run_demo` 到不同目录后比对全部 299 个文件的 sha256：
**byte-different files: []**，即相同种子与操作序列下导出动画与日志逐字节一致。

## 5. 复现实验命令

```bash
pip install -r requirements.txt
python -m unittest discover -s tests
python scripts/run_demo.py --workspace outputs/demo --gui
python scripts/cli.py generate --out samples/my --seed 7 --joints 12 --frames 20
python scripts/cli.py retarget --source <src.json> --target <tgt.json> --out outputs/one
python scripts/cli.py gui --source <src.json> --target <tgt.json> --offscreen
```

## 6. 已知边界

* GUI 在无显示服务器的环境使用 Qt offscreen 平台做构建自检；
  Windows 本地桌面直接运行即可进入真实播放界面。
* v1 本身不携带层级，按确定性线性链推导并在 provenance 标注
  `derived:v1_linear_chain`，升级后参与与 v2/v3 相同的全局空间重定向。
