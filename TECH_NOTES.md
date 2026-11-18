# 技术说明与实测结果

## 确定性设计

- 每轨使用独立 `numpy.random.Generator(SeedSequence(seed))`（PCG64），种子来自
  清单整数；共享节奏脉冲为无随机解析公式，组内真值可复算。
- 所有 DSP 使用 float64、固定滤波阶数与 NumPy/SciPy 确定性算子；母带统一
  PCM-16 写出，重复运行 SHA-256 一致（测试 `test_master_render_is_byte_identical_across_runs`）。

## 解析双耳模型

- ITD：`itd = 1.5 * head_radius / c * sin(azimuth)`，Woodworth 连续形式。
- ILD：`ild_db = 2.8 * sin(azimuth)`，配合功率守恒正弦声像律；2.8 的系数保证
  相邻 10° 最大 ILD 步进约 0.49 dB，低于默认 0.5 dB 稳定阈值。
- 仰角/距离仅引入平滑公共增益，不扰动耳间差；左右耳延迟用流式线性插值
  可变延迟线，不存在离散左右分支，因此无整侧翻转。
- ILD 本身存在前后混淆（cone of confusion），报告用解析姿态的前后半球标记
  解析主声像方位，逐角度真值偏差实测为 0°。

## 瞬态与相位

- 瞬态检测在参考点 ±0.12 s 窗口取绝对幅度峰值，回退到 20% 峰值穿越；极性
  翻转不影响检测。环境床峰均比不足 5 倍时返回 `None` 并产生
  `DETECTION_FAILED`，该轨不偏移且继续参与混音。
- 同组轨嵌入同相 60 Hz 节奏脉冲与单采样发射标记；演示中衣料轨注入极性
  翻转。低频带通（20–120 Hz）零相位相关估计极性与残差延迟，校正后强节奏
  段低频能量实测提升约 12 dB（≥ 6 dB 要求），瞬态位移 0 采样。

## 响度与真峰值

- K 加权由 RBJ 双线性变换构造（1.5 kHz 高架 +4 dB、38.135 Hz 高通），按
  BS.1770-4 的 400 ms/100 ms 块与双门限计算积分响度。
- 真峰值用 4 倍多相过采样；限幅器以过采样峰值包络驱动，含 0.1 dB 安全余量，
  单采样前瞻、长度不变。三次“归一化→限幅”迭代后实测：
  积分响度 -16.25 LUFS（目标 -16，误差约 -0.25 LU），真峰值约 -1.07 dBTP
  （上限 -1.0 dBTP）。

## 快照与中断

- 块大小由清单 `block_frames` 给定（示例 5 帧 × 0.1 s = 0.5 s/块）。
- 每块先写 `block_xxxxxx.npy`，再以 `os.replace` 原子替换 `snapshot.json`；
  快照含清单 SHA-256、参数摘要、已完成块数与瞬态/相位参数。
- SIGINT 处理器只置位，当前块完成后提交快照并写 `recovery_report.json`，
  进程退出码 0；续算时每块带零填充上下文前缀独立渲染，消除延迟线历史依赖，
  实测续算母带与一次性渲染 SHA-256 完全相同。

## 实测命令与结果

```text
$ python -m pytest
20 passed in 6.40s

$ python -m offline_master.demo invalid-demo ...
exit=2, E_MANIFEST_INVALID, 11 条结构化告警

# Ctrl+C 后续算
sha256sum full/master.wav resume/master.wav
62582109d455a67a451da1fa8c04cc5481fd3dd851186c9b58b8fc26c74c2139  full
62582109d455a67a451da1fa8c04cc5481fd3dd851186c9b58b8fc26c74c2139  resume
```
