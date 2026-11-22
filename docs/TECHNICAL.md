# Resona Restore 技术说明

## 1. 模块结构

| 模块 | 职责 |
| --- | --- |
| `config.py` | 冻结配置、范围校验、音频类型默认参数、配置指纹 |
| `audio_io.py` | WAV 解码、float64 归一化、不可变音频容器、原子导出 |
| `dsp.py` | 分帧 STFT、噪声估计、谱减、掩膜平滑、加权 OLA/ISTFT |
| `segments.py` | 固定采样边界、四态状态机、严格时间轴拼接 |
| `checkpoint.py` | 快照身份、原子 NPZ 提交、manifest、整体失效 |
| `engine.py` | 有界并行调度、暂停/停止/恢复、三路径、段预览与导出 |
| `metrics.py` | 峰值/RMS/门限响度、float64 输出哈希、首差定位 |
| `verification.py` | 端到端验收与报告 |
| `samples.py` | 固定种子歌曲/旁白/边界样本 |
| `ui/widgets.py` | 自绘波形与前后频谱 |
| `app.py` | PySide6 主窗口、QThread 工作器、试听、导出、审计 |

## 2. 分帧 STFT、ISTFT 与精确时长

每段输入长度为 `N`，帧长 `L`，帧移 `H`。实现不依赖第三方 STFT 的默认裁边：

1. 左侧固定填充 `L/2`。
2. 帧数：

   \[
   n_F = 1 + \max\left(0,\left\lceil\frac{N + 2(L/2)-L}{H}\right\rceil\right)
   \]

3. 根据最后一帧覆盖范围计算最小右侧填充，保证源时间轴全部被覆盖。
4. 以 `np.lib.stride_tricks.sliding_window_view(..., L)[::H]` 提取帧，避免复制前先做越界切片。
5. 分析窗 `w` 后做 `rfft`。
6. 逆变换得到帧，乘同一合成窗 `w` 后逐帧 OLA。
7. 每个采样点除以窗口平方能量 `sum(w^2)`。
8. 从左侧填充偏移处裁出恰好 `N` 点。

若目标区间存在零窗口能量，不做静默补零，而是返回 `E_PARAM`。这样 `H=L` 的矩形窗可工作，而无法覆盖的 Hann/Hamming 组合会显式失败。

实测 `N={1,7,1000,8193,16385}`、单/双声道、Hann/Hamming/Blackman/矩形窗均输出精确 `N` 点；0 强度重建最大误差约 `1e-16`。

## 3. 噪声谱估计与谱减

对每段复数谱 `X[f,t,c]`：

1. 计算功率 `P=|X|^2`。
2. 噪声模式：
   - `leading`：前 `noise_frames` 帧均值；
   - `silence`：在开头搜索窗口内取帧能量最低 20% 的均值。
3. 噪声功率轮廓 `N[f,c]`，与音频同样按声道独立。
4. 原始增益：

   \[
   g_0 = \sqrt{\mathrm{clip}(1-\alpha N/P,\; floor^2,\;1)}
   \]

   `alpha` 即 UI 强度。每个频点增益随 `alpha` 单调不增。
5. 频率方向使用 edge 模式滑动平均；时间方向使用一阶因果平滑：

   \[
   g_t=(1-\beta)g'_t+\beta g_{t-1}
   \]

6. 修复谱 `X*g` 进入同一个 ISTFT。

因为谱减强度只通过单调增益进入线性 ISTFT，实际输出 RMS 在固定样本上单调不增；验收记录了强度 `0, 0.5, 1, 1.5, 2, 2.5, 3, 4` 的音频输出 RMS 与平均增益。

默认预设：

| 参数 | 音乐 | 旁白 |
| --- | ---: | ---: |
| 帧长/帧移 | 2048/512 | 1024/256 |
| 窗 | Hann | Hann |
| 强度 | 1.35 | 1.85 |
| 增益下限 | 0.04 | 0.06 |
| 噪声帧 | 12 | 8 |
| 频率/时间平滑 | 5 / 0.25 | 7 / 0.5 |

## 4. 分段与状态机

分段边界为整数采样点：

```text
[start, end), end = min(total_samples, start + segment_samples)
```

因此尾段可以短于固定段长，不产生补零输出。导出前要求所有段都存在且 `start` 严格衔接上一段 `end`，最终游标等于源总采样数。

合法迁移：

```text
PENDING    -> PROCESSING
PROCESSING -> COMMITTED
PROCESSING -> FAILED
FAILED     -> PROCESSING
```

`COMMITTED` 为终态。非法迁移抛 `E_STATE`，在锁内先判断后修改，所以原状态不变。检查点恢复不是实时迁移，而是经过完整校验后直接初始化 `COMMITTED` 记录。

## 5. 检查点协议

目录：

```text
<checkpoint>/
  snapshot/
    manifest.json
    segments/
      00000000.npz
      00000001.npz
```

快照身份包含：

- `schema_version`
- `pipeline_version`
- 源 WAV SHA-256
- 采样率、声道、总采样点数
- 分段采样数、总段数
- 由上述内容计算的确定性 `snapshot_id`

每个 `.npz` 保存：

- 完整修复段 `float64[N_seg, channels]`
- 段起始/结束采样点与采样点数
- 快照身份、配置、配置指纹
- 输出 SHA-256
- 提交时间与 DSP 统计

### 提交顺序

工作线程只计算，不写检查点。主调度线程按升序接收同一批的 future，并逐个执行：

1. 写入同目录临时文件。
2. `flush + fsync`。
3. `os.replace` 原子发布 `.npz`。
4. `fsync` 目录。
5. 原子更新 manifest。
6. 段状态迁移到 `COMMITTED`。

NPZ 原子替换是耐久提交点。若强杀发生在 manifest 更新前，重启会扫描并校验 sidecar 后重建索引；若 NPZ 或元数据损坏，则整个快照作废，不会只取其中几段。

### 版本失效

加载时先校验 manifest 身份，再在返回任何段之前校验所有 sidecar。以下任一情况会将整个 `snapshot/` 移到 `invalidated-时间-随机后缀/`：

- schema/pipeline 版本不符；
- 音频哈希或时间轴身份不符；
- snapshot id 不符；
- manifest 引用缺失段；
- sidecar 元数据、形状、配置指纹或输出哈希不符。

随后创建全新快照，所有段为 `PENDING`。

### 参数变更

快照身份不包含用户 DSP 参数，因此处理中途可以修改参数。每个段的 sidecar 保存它自己的配置和指纹。已提交段不会被重写；新参数只用于下一批未提交段。重启后已提交段仍按原配置输出，未提交段使用 UI 当前参数。

## 6. 三路径一致性

三条路径：

1. **实时处理**：`process_realtime_with_checkpoint`，单工作线程，逐段提交。
2. **断点恢复**：先提交部分段，销毁会话，再从持久化检查点加载并继续。
3. **全量重算**：`process_full_recompute`，不读/写检查点，按相同分段与并行上限计算。

三者调用同一个 `process_segment -> denoise_segment` 纯函数，输入切片仅由 `SegmentSpec` 决定。DSP 不依赖时钟、随机数、全局状态或调度顺序。最终输出按固定索引拼接，因此数组逐位相等，峰值、RMS、集成响度和输出哈希也完全相等。

`locate_first_difference` 对首个不同采样帧定位 `(sample, channel, segment_index)`；验收另用人为扰动证明定位器有效。

## 7. 并发与资源边界

- `max_workers` 限制为 1..8。
- 每批最多 `max_workers` 段，只有批内计算并行。
- `max_buffer_bytes` 下限 4 MiB；按 `workers * segment_samples * channels * 8 * 8` 的保守峰值估算，超过预算返回 `E_PARAM`。
- 检查点写入只在会话线程进行，并有锁；并发实测最大写入数 1。
- 批内提交按升序，跨批也按升序；导出再从磁盘按索引重建。
- UI 工作器是 `QThread`，信号回主线程更新控件；处理自身使用有界 `ThreadPoolExecutor`。

## 8. 暂停、停止与异常

- 暂停：设置事件；当前批完成后停止提交，等 `PROCESSING` 集合为空后进入 `PAUSED`。
- 恢复：清除暂停事件，继续待处理段。
- 停止：当前批完成后退出，只保留已提交段；结果标记 `STOPPED`，不生成完整报告。
- 强杀：无法执行进程内清理；重启仅扫描已原子发布的段。
- 单段异常：捕获为 `E_INTERNAL` 或业务码，段置 `FAILED`，其他段继续；`retry_failed=True` 可从失败态转回处理态。
- 损坏/不可解码文件：解码入口统一返回 `E_DECODE`。

## 9. 响度统计

`metrics.integrated_lufs` 使用：

- 100–12000 Hz（受奈奎斯特限制）二阶带通近似 K 加权；
- 400 ms、100 ms 步长块；
- -70 LUFS 绝对门限与 -10 LU 相对门限。

结果是确定性的 BS.1770 风格比较值，不是认证级广播响度计。三路径比较依赖同一实现和同一采样数组，因此可逐位复核。

## 10. 自动化验收

`scripts/run_acceptance.py` 自动完成：

- 样本 manifest 哈希与重生成字节一致性；
- 四种窗/帧移、短输入、尾段、单双声道重构；
- 强度单调性；
- 歌曲和旁白的实时/恢复/全量逐样本比较；
- 峰值、RMS、LUFS、输出哈希比较；
- 首差定位器扰动验证；
- 子进程 `os._exit(27)` 强杀恢复；
- manifest 版本篡改整体失效；
- 暂停/恢复；
- 注入段故障与重试；
- 非法迁移 `E_STATE`、参数 `E_PARAM`、损坏文件 `E_DECODE`；
- 参数变更不重写已提交段；
- 有界并发下检查点串行与升序提交。

`tests/test_core.py` 使用 `unittest` 覆盖同一批核心不变量。`scripts/ui_smoke.py` 在 `QT_QPA_PLATFORM=offscreen` 下完成真实 UI 处理并保存截图。

## 11. 运行限制

- 应用不访问网络；依赖安装是一次性环境准备。
- 当前验收在 Linux/WSL 完成，未在原生 Windows 实机重跑。
- 同一检查点目录应在单应用进程内使用；跨进程同时驱动同一快照不在当前保证范围。
- 所有段独立估计噪声轮廓。这样保证断点恢复和并行不受跨段上下文影响；三路径一致性优先于跨段全局噪声建模。

