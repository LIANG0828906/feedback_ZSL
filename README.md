# 戏曲古戏台声景离线母带渲染组件

从零构建的确定性离线母带渲染管线：合成六路同期声 → 解耦掩蔽串扰 → 瞬态整数采样对齐
→ 分段立体声混音 → BS.1770 响度/真峰值合规 → 分块快照与崩溃恢复。

## 快速开始

```bash
python -m pip install -r requirements.txt
python -m opera_master demo-cue demo
python -m opera_master generate demo/cue.json
python -m opera_master render demo/cue.json
```

产物：

- `demo/sources/*.wav`：六路确定性合成素材（板鼓/大锣/小锣/帮腔/月琴/底噪参考）
- `demo/out/master.wav`：立体声母带
- `demo/out/decouple_report.json`：逐通道解耦质量报告与分段响度/真峰值
- `demo/out/snapshots/`：分块快照（成功渲染后自动清理）

## 测试

```bash
python -m unittest discover -s tests -t .
```

35 个用例覆盖清单校验、确定性合成、串扰解耦、瞬态对齐、母带合规、报告逐字节一致性、
SIGKILL/SIGINT 快照恢复与 `E_RESUME_MISMATCH` 处理。

算法口径、业务码与实测结果详见 `TECH_NOTES.md`。
