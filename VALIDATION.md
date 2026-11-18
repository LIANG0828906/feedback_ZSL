# 实际验证记录

验证日期：2026-11-18（Asia/Shanghai）

本机环境：

- Python 3.14.4
- NumPy 2.5.5
- Linux 开发环境；实现只使用 Python 标准库与 NumPy，路径和无容器约束适用于 Windows 10/11

## 自动测试

命令：

```bash
.venv/bin/python -m unittest discover -s tests -t . -v
```

结果：

```text
Ran 23 tests in 8.383s
OK
```

覆盖项包括：

- 索引越界、退化三角形、透光率/反射率越界、重复片身份、重复象限声明。
- 校验失败退出码 `2`，且不产生 `rgb.png` / `rgb.npy`。
- 纹理正序/逆序生成逐字节一致，同色系 6 片纹理摘要互不相同。
- 蓝片未粉化、侧向红片色相和亮度可辨认。
- 每片透射/反射贡献占比之和为 1，最终 RGB 严格位于 `uint8` 范围。
- 投影灰度剖面重复生成逐字节一致，过渡宽度随深度严格增加。
- 归属图、投影落点、PNG 编码确定性。
- 人为修改一个像素后，差异报告包含块号、像素坐标和差异通道。
- `SIGKILL` 后续算、`SIGINT` 当前块提交、损坏快照及场景摘要不匹配时整体丢弃。

## 完整样例指标

场景：`scenes/rose_window.json`

- 场景摘要：`90b277e3ef213489da17beec865c3843ac3553a84c5c06b7e7e8fa0590a4cc56`
- 图像：`640 x 400`，块大小 `32`，共 `260` 块
- 玻璃片：`24` 片，4 个象限
- 去重铅条投影边：`96` 条
- 正对光源蓝片平均 RGB：`[90.08, 133.96, 214.96]`，蓝通道占优
- 侧向红片平均 RGB：`[214.95, 77.55, 72.00]`，红色相保留
- 反射贡献占比范围：`0.0087591` 到 `0.03775408`
- 合成后最大线性通道：`1.0`（保色相压缩后）
- 最终 RGB 最大通道：`215`，无溢出
- 软阴影过渡宽度（深度递增）：`0.16275`, `0.186375`, `0.21`, `0.23625`
- `transition_width_monotonic_with_depth = true`

基线输出位于 `artifacts/baseline/`。关键 SHA-256：

```text
6f83377fc425404b67a5cdde3c824f4cef42f0e0fcb77219bee7e4c6de98ee1e  rgb.npy
b23ca2beff298517a594da046d0cb5815d38b77084195107079292d0fd2e8e18  ownership.npy
c0c445e093aedd424fc65a90980dac17353b9df07e1b45aa22512315eeaef932  rgb.png
b6f81fc6e2f8f3e6822b0b40d27d6ab3fbdcd7b849fa8fb4eb52a8ff1c305140  ownership.png
681615dd6b866f965a7bf9b6b9de272e9e7f79e3fd810675dcb1b833db0ef5cf  report.json
fcf9531f7957a3a17fdb13d6771243050db2c6f74789f013698003ab8d14218d  shadow_projections.json
```

## 强制终止续算

另行启动了带 `--chunk-delay-ms 18` 的完整渲染，在快照中观察到 12 个已提交块后发送 `SIGKILL`：

```text
observed_committed_chunks = 12
SIGKILL returncode        = -9
snapshot_exists           = true
```

随后执行相同命令恢复：

```text
snapshot_load_status = resumed
resumed_chunk_count  = 12
computed chunks      = 248
exit code            = 0
```

恢复结果与 `artifacts/baseline/` 比较：

```text
mismatch_count = 0
differences    = []
```

比较项包括 `rgb.npy`、`ownership.npy`、`rgb.png`、`shadow_projections.json`、`ownership_map.json` 和稳定报告 `report.json`。完整机器可读摘要见 `artifacts/validation_summary.json`。

## 可处理中断

自动测试另外验证了 `SIGINT`：

- 进程完成当前块、提交快照后退出码为 `0`。
- 写出 `recovery_report.json`，其中 `snapshot_committed = true`。
- 中断时未写出最终 `rgb.npy`。
- 重启后续算，与完整运行比较 `mismatch_count = 0`。

## 损坏与场景不匹配

- 将快照替换为损坏字节后重启：自动记录 `discarded_corrupt`，从块 0 重算并成功完成。
- 快照载荷哈希、逐块颜色摘要、尺寸/块参数和场景摘要均有校验；任一不一致都不会沿用旧块数据。
