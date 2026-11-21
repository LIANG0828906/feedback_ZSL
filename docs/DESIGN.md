# 技术设计与验收映射

## 分层

| 层 | 模块 | 职责 |
| --- | --- | --- |
| 格式层 | `volume.py` | 128 字节头、定长小端字段、密度/掩码读取、长度与字段校验 |
| 映射层 | `volume.py` | 窗宽窗位到 `[0,1]`、归一化参考、全体体素差异统计 |
| 传递函数层 | `transfer.py` | 严格递增控制点、分段线性 RGBA LUT、失败保留旧 LUT |
| 参数层 | `params.py` | 不可变参数组、规范 JSON、SHA-256 内容哈希 |
| 渲染层 | `renderer.py` | 光线投射、三线性采样、梯度光照、前向 alpha 合成、掩码叠加 |
| 输出层 | `buffers.py` | 颜色缓冲与清单、内容哈希验真 |
| 状态层 | `preview.py` | 已提交显示帧与未提交拖动参数分离 |
| 快照层 | `snapshot.py` | 版本化快照、原子保存、恢复/重渲染判定 |
| 调度层 | `tasks.py` | 每体数据单飞、协作取消、失败不提交 |
| 产物层 | `pipeline.py`, `verification.py` | 样本生成、固定演示、验收报告 |
| 桌面层 | `app.py` | PyQt5 窗宽窗位、TF 编辑、提交、快照载入 |

## 关键状态不变量

### 参数组

`RenderParameters` 冻结且只保存不可变 tuple。每次字段变化通过 `with_changes` 生成新对象和新哈希，不在旧对象上原地修改。

参数哈希输入包含浮点十六进制表示，避免 JSON 数字格式差异；同时覆盖窗宽窗位、控制点与步长，保证三者的任意变化都可区分。

### 预览

```text
DisplayedFrame(params=P, result=R)
    P.parameter_hash == R.parameter_hash
    P.viewport == (R.width, R.height)

set_pending(N)
    DisplayedFrame 不变
    pending = N, pending_is_uncommitted = true

commit(N, R2)
    校验 N.parameter_hash == R2.parameter_hash
    校验成功后一次性替换 DisplayedFrame
```

`commit` 校验失败时旧 `DisplayedFrame` 不变。GUI 的已提交标签完全从 `DisplayedFrame.params` 渲染，滑块值只用于待提交标签。

### 缓冲

```text
manifest.parameter_hash == params.parameter_hash
manifest.parameters content hash == manifest.parameter_hash
sha256(rgba bytes) == manifest.buffer_hash
len(rgba bytes) == width * height * 4
```

只有全部成立才返回缓冲。读取逻辑不把 mtime 或文件名当作参数身份。

### 快照恢复

恢复时按顺序执行：

1. 快照 schema/version/`snapshot_id`/参数哈希校验；
2. 请求参数与快照参数逐字段比较；
3. 若完全一致，读取缓冲清单并重新验真；
4. 缓冲参数、内容、覆盖轨迹全部匹配才复用；
5. 否则以请求参数重新离屏渲染、发布缓冲，再原子写新快照。

## 掩码差异证明

渲染器逐样本记录 `mask_coverage[pixel]`：光线任一样本落入标记体素即为真。无掩码与有掩码版本只在下列条件下产生颜色差异：

- 样本位置相同；
- 标量、窗口映射、LUT、梯度、步长相同；
- 仅掩码样本改变该样本颜色/alpha。

因此任何颜色差异像素必定满足 `mask_coverage[pixel] = true`。验收报告分别统计掩码覆盖像素、改变像素、覆盖外改变像素，并要求覆盖外计数为 0。

## 取消与资源

光线按视口行批次处理，每批次和每采样步均检查取消事件。取消异常在发布颜色缓冲之前抛出，因此：

- 不生成新颜色缓冲清单；
- 不写新快照；
- 旧快照字节不变；
- NumPy 临时数组随栈释放；
- 文件写入使用局部上下文管理器。

服务不生成“强制终止退出报告”；崩溃恢复入口只接受磁盘快照。

## 验收项映射

| 业务要求 | 实现 | 自动化检查 |
| --- | --- | --- |
| 头解析、长度、间距、窗映射 | `volume.py` | `64/128:header_parse`、`*:header_invalid_code` |
| 窗变化差异统计 | `compare_window_mappings` | `test_window_delta_reports_all_voxels`、demo `window_mapping_delta` |
| TF 校验与旧 LUT 保留 | `transfer.py` | `test_invalid_update_preserves_previous_lut` |
| 参数哈希覆盖三项 | `params.py` | `test_hash_covers_window_controls_and_step` |
| 离屏重复渲染字节一致 | `renderer.py` | `*:offscreen_byte_repeatability` |
| 参数/缓冲哈希验真 | `buffers.py` | `*:param_stale_rejection` |
| 预览与离屏严格对齐 | `preview.py`, `app.py` | `*:preview_offscreen_alignment` |
| 快照原子、版本、恢复重渲染 | `snapshot.py` | `*:snapshot_reuse_and_rerender`、`*:snapshot_invalid_no_output` |
| 并发 E_BUSY、协作取消 | `tasks.py` | `concurrency:duplicate_is_busy`、`concurrency:cooperative_cancel` |
| 掩码一致性与越界错误 | `renderer.py`, `volume.py` | `*:mask_overlay_consistency`、`*:mask_invalid_code` |
| 确定性样本/缓冲/日志 | `synthetic.py`, `pipeline.py` | `determinism:generation_render_trace` |
| 强制终止后恢复 | `snapshot.py` | `*:crash_recovery_from_snapshot` |

## 实际验证摘要

验收在 `output/samples/` 下的本地确定样本上运行，覆盖 64³ 与 128³。检查通过后写入：

- `output/reports/verification.json`：机器可读明细；
- `output/reports/verification.md`：逐项 PASS/FAIL；
- `output/reports/baseline.json`：两级基线参数哈希与缓冲哈希；
- `output/traces/demo_64.jsonl`：固定操作序列中的提交、待提交标注、掩码一致性与快照恢复事件。

报告以实际运行结果为准，不在文档中硬编码会随代码微调变化的哈希。
