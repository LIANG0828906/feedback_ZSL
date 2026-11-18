# 验证记录（本地确定性样本）

## 1. 环境

| 项 | 值 |
| --- | --- |
| 解释器 | CPython 3.11.18（`uv venv --python 3.11`） |
| 依赖 | NumPy 1.26.4，无其他第三方库 |
| 硬件 | 仅 CPU，无 GPU/网络/外部素材 |
| 场景 | `build_default_scene` 代码确定性生成：3 层 × 12 砖 = 36 块、3 火口、5 个严格递增视角 |
| 图像 | 96×72，分块 32×24，软阴影采样密度 16，气泡样本 6/砖 |

## 2. 测试执行

命令：

```bash
. .venv/bin/activate
python -m unittest discover -s tests
```

结果（最近一次完整运行）：

```
Ran 36 tests in 368.256s
OK
```

36 个用例分四组：

- `tests/test_xrand_fringes.py`（7）：SplitMix64 黄金值、纯函数随机流、
  条纹/气泡逐字节复算、与遍历顺序无关、相邻砖可区分。
- `tests/test_validation.py`（10）：11 类业务错误码中的代表性非法输入、
  多问题同时收集、默认场景合法。
- `tests/test_render_pipeline.py`（10）：视角产物完整性、归属图仅凭场景复算、
  重复运行逐字节一致、相机移动不改几何、三火口落点可区分、出界无硬黑边、
  半影单调且分布确定、无过曝、无闪烁碎点、层距变化落点偏移。
- `tests/test_snapshot_resume.py`（5）+ `tests/test_cli_integration.py`（4）：
  损坏快照丢弃、种子变更失效、摘要/颜色摘要、中断后续算逐字节一致、
  已提交分块丢失整体重算、CLI 退出码、真实子进程 SIGINT 优雅退出码 0。

## 3. 关键实测数据

### 3.1 端到端渲染与确定性

- 5 视角完整渲染耗时约 35.7 秒（96×72、16 阴影样本）。
- 两次独立完整输出的目录级 SHA-256（相对路径 + 文件字节）完全相等：
  `tree digest equal: True`。
- 同砖条纹表重复生成字节相等：`fringe bytes equal: True`。
- 视角 2 玻璃片元占比 28.7%，其余为砖缝/背景；归属图取值均为场景内砖编号或 -1。

### 3.2 逐层透射/散射贡献占比（render_report.json）

| 层（内→外） | 透射占比 | 散射占比 | 片元数 |
| --- | --- | --- | --- |
| 0 | 0.523824 | 0.476176 | 62 |
| 1 | 0.249384 | 0.750616 | 658 |
| 2 | 0.248498 | 0.751502 | 9283 |

外层砖散射主导，配合环境条纹底光保证背光面仍可辨认应力条纹。

### 3.3 砖缝落点

- `grout_landings.json` 共 135 条（9 条砖缝 × 3 火口 × 3 个沿线采样点），
  默认接收面下 135 条全部在界内。
- 同一砖缝在三火口下的落点互不相同，例如某竖缝 50% 采样点：
  火口 0 `(-0.5109, 0.2261)`、火口 1 `(-1.1550, -0.2919)`、
  火口 2 `(-1.3681, 0.0118)`。
- 层整体外移 0.08 后，落点发生一致偏移（`test_landing_shifts_...`）。

### 3.4 半影宽度单调趋势（band_trend.json，火口 1）

| 层 | 火口→砖缝距离 d_lg | 砖缝→接收面距离 d_gr | 半影宽度 w |
| --- | --- | --- | --- |
| 0 | 2.30 | 0.97 | 0.128312 |
| 1 | 2.65 | 0.62 | 0.118923 |
| 2 | 2.95 | 0.32 | 0.112649 |

固定火口跨层比较时，随 `d_gr` 增大宽度单调变宽（0.112649 → 0.118923 →
0.128312）；固定层跨火口比较时，随 `d_lg` 增大同样单调变宽。
`trend_check.monotonic = true`。11 点亮度剖面为平滑高斯形：
`[4e-06, 0.000335, 0.011109, 0.135335, 0.606531, 1.0, 0.606531, 0.135335,
0.011109, 0.000335, 4e-06]`，无锯齿硬边。

### 3.5 动态范围与抗闪烁

- 全部 5 视角 RGBA 通道均在 `[0,255]`，玻璃区域最大通道 < 245，无惨白过曝。
- 重复渲染逐像素相等；孤立极亮单点扫描结果为 0，无随视角/遍历顺序变化的碎点。

### 3.6 中断与续算

- 计划中断（5 块后）与真实 SIGINT 子进程均返回退出码 0，存在
  `progress_snapshot.json` 与 `recovery_report.json`。
- 续算完成后 `render_report.json.restarted_from_snapshot = true`，
  `compare` 对不中断参考运行返回 `status=identical`、差异数 0。
- 人为损坏快照 JSON 或修改任一条纹种子后，快照被删除并从首个视角重算。

## 4. 退出码约定

| 码 | 含义 |
| --- | --- |
| 0 | 成功；或 SIGINT/计划中断在块边界优雅结束 |
| 2 | 场景校验失败（结构化错误码报告） |
| 3 | 续算与参考运行存在逐字节差异（结构化差异报告） |
| 4 | 运行期错误（如网格/AABB 归属不一致） |

## 5. 复现步骤

```bash
uv venv --python 3.11 .venv && . .venv/bin/activate
uv pip install "numpy==1.26.4"
python -m kiln_glass generate --scene artifacts/scene.json
python -m kiln_glass validate --scene artifacts/scene.json
python -m kiln_glass render  --scene artifacts/scene.json --output artifacts/run_full
# 中断-续算一致性演示
python -m kiln_glass render  --scene artifacts/scene.json \
    --output artifacts/run_resume --interrupt-after-tiles 10
python -m kiln_glass render  --scene artifacts/scene.json --output artifacts/run_resume
python -m kiln_glass compare --reference artifacts/run_full \
    --candidate artifacts/run_resume --report artifacts/diff.json \
    --scene artifacts/scene.json
```
