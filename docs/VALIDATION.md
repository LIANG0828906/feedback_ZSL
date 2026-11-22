# 验收运行记录

环境：Python 3.14.4（uv 创建 .venv），NumPy 2.5.5，PySide6 6.12.0，pytest 9.2.0；无容器。
样本：固定种子 20261123 生成 3 条工程（各 8 段语音 / 8 句台词，16kHz，帧长 400，帧移 160）。

```text
===== 1. 生成确定性样本（seed=20261123） =====
[gen] demo/samples/proj_0/manifest.json  segments=8 lines=8
[gen] demo/samples/proj_1/manifest.json  segments=8 lines=8
[gen] demo/samples/proj_2/manifest.json  segments=8 lines=8

===== 2. 工程摘要与台词总数 =====
{
  "project_id": "proj_0",
  "sample_rate": 16000,
  "frame_len": 400,
  "hop": 160,
  "segments": 8,
  "total_lines": 8,
  "total_chars": 141,
  "timeline_samples": 280022,
  "param_version": "pv1",
  "content_hash": "263aaf3373a691f8"
}
===== 3. 串行 vs 并行（8 workers）逐句一致性 =====
[done] 已提交 8/8 句台词
[done] 已提交 8/8 句台词
串行结果 == 并行结果（窗口/折行/取舍逐句一致）: True
含取舍记录的台词: [('L001', ['reclaim_gap']), ('L002', ['reclaim_gap']), ('L004', ['reclaim_gap']), ('L006', ['reclaim_gap']), ('L007', ['reclaim_gap'])]

===== 4. 独立验证工具（三个工程） =====
proj_0: 验证通过: 8/8 句台词，窗口不重叠、阅读速度达标、折行与真值一致、总时长在容差内
proj_1: 验证通过: 8/8 句台词，窗口不重叠、阅读速度达标、折行与真值一致、总时长在容差内
proj_2: 验证通过: 8/8 句台词，窗口不重叠、阅读速度达标、折行与真值一致、总时长在容差内
===== 5. 断点续跑（提交 3 句后退出，重启续跑） =====
[done] 已提交 3/8 句台词
--- 重启 ---
[done] 已提交 8/8 句台词

===== 6. 强制终止（kill -9）恢复边界 =====
kill 后已持久化（恢复边界）: ['L001', 'L002', 'L003']
[done] 已提交 8/8 句台词
kill 恢复结果 == 全新单次运行: True
```

```text
===== 7. 异常码链路演示 =====
E_SCHEMA 字段缺失: [E_SCHEMA] manifest 缺少必需字段 (field: frame_len)
E_REF 引用悬空: [E_REF] 台词引用的语音段不存在（悬空引用） (field: subtitles[0].segment_id)
E_REF 窗口跨段: [E_REF] 显式显示窗口超出所在语音段（跨段） (field: subtitles[0].window)
E_PARA 行宽为零: [E_PARA] 参数必须为合规正整数 (field: presentation.max_chars_per_line)
E_PARA 语速非正: [E_PARA] 阅读速度上限必须为正 (field: subtitles[1].max_cps)
E_STATE 非法迁移: [E_STATE] 非法状态迁移 PENDING -> COMMITTED

--- E_FIT 冲突不可收敛 ---
[E_REF] 清单引用的音频文件不存在 (field: audio_file)

--- E_SEG 坏段链路（验证工具，可重试不污染） ---
[E_SEG] 台词 L002 违规项 segment_ref 实测值 [0, 65489]  -> 走 E_SEG 链路，可修复后重试，其他台词不受影响
[done] 已提交 8/8 句台词
验证通过: 8/8 句台词，窗口不重叠、阅读速度达标、折行与真值一致、总时长在容差内
```

E_FIT 演示（注入超长文本 + 极低语速 + 无空隙，其余台词不受影响）：
```text
[E_FIT] 排版与可读时长冲突无法收敛
{"lines": [{"line_id": "L001", "deficit_samples": 1896640, "screens": 1}]}
[commit] L002 ... L008 正常提交
```

```text
===== 8. 快照版本不匹配整体作废 =====
[done] 已提交 5/8 句台词
--- 同一持久化目录切换参数版本 pv2 ---
8
（8 条 commit = 旧快照作废后全量重算）

===== 9. pytest 全量 =====
.................................                                        [100%]
33 passed in 1.65s
```

GUI：DISPLAY=:0 下 `.venv/bin/python gui.py demo/samples` 窗口正常启动（6 秒超时无报错退出）；
无图形界面时使用 main.py 全部子命令（offscreen 冒烟测试见 tests/test_gui.py）。
