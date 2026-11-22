# 赛事直播本地转码审看台

这是一个完全本地、确定性的多声道 WAV 分档转码准备实现，覆盖：

- 48 kHz、16/24-bit PCM、mono/5.1 WAV 解析与字段校验；
- 多档目标采样率、滤波器长度、量化位深与单调参数版本；
- 跨切片延续滤波历史的确定性多相重采样、固定样本切片与相位余量；
- 逐片侧信道、内容哈希、峰值/静音标记、哈希链和样本级一致性审计；
- 参数变更后的部分复用、相位强制失效传播与轨迹记录；
- 不可变版本发布、原子清单切换、查询隔离与 `E_VERSION_STALE`；
- 按切片原子提交、SIGKILL 后哈希链校验、相位衔接续转和 GUI 审看。

## 环境与离线安装

工作区已准备本地轮子目录 `wheelhouse/`（作为本地缓存，不纳入 Git 跟踪），
使用 Python 3.11 安装：

```bash
./setup_offline.sh
```

安装结果为 Python 3.11.18、NumPy 2.2.6、PySide6 6.8.3。安装命令使用
`--no-index --find-links wheelhouse`，运行与测试不再访问网络。

## 快速运行

```bash
# 1. 生成固定种子的 4 个样本：mono/5.1 × 16/24-bit
./run.py generate --out artifacts/samples --seed 19093 --duration 2.4

# 2. 对一档生成切片、侧信道并发布
./run.py transcode \
  --wav artifacts/samples/surround51_24bit_seed19093.wav \
  --store artifacts/review_store \
  --tier hi44 --rate 44100 --filter 48 --quant 16 --slice-samples 1024

# 3. 从已发布清单逐片审计
./run.py audit \
  --wav artifacts/samples/surround51_24bit_seed19093.wav \
  --store artifacts/review_store --tier hi44

# 4. 查看发布清单
./run.py status --store artifacts/review_store

# 5. 图形界面；无显示环境可加 QT_QPA_PLATFORM=offscreen
./run.py gui --store artifacts/review_store --samples artifacts/samples
```

同一 `--wav/--store/--tier` 再次执行 `transcode` 会读取快照并续转，参数与源文件
哈希必须一致。

## 测试与验收产物

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python \
  -m unittest discover -s tests -t . -v

.venv/bin/python scripts/run_acceptance.py
```

`scripts/run_acceptance.py` 会自动重跑单测、离屏 GUI 冒烟与截图，再执行分档、
相位传播、漂移归因、发布隔离、SIGKILL 恢复和确定性验证。当前实际结果：

- unittest：`23/23` 通过，包含真实 `SIGKILL` 子进程恢复与离屏 Qt GUI 工作流；
- 验收报告：`artifacts/acceptance/report.json`、`artifacts/acceptance/report.md`；
- unittest 原始输出：`artifacts/acceptance/unittest.log`；
- 验收脚本完整输出：`artifacts/acceptance/acceptance.stdout.json`；
- 主轨迹日志：`artifacts/acceptance/trace.jsonl`；
- 强杀恢复轨迹：`artifacts/acceptance/recovery_store/trace.jsonl`；
- 固定样本：`artifacts/acceptance/samples/`；
- 侧信道/缓冲漂移样例：`artifacts/acceptance/drift_cases/`；
- 离屏 GUI 冒烟截图：`artifacts/acceptance/gui_smoke.png`。

本次实际验收摘要：

| 项目 | 结果 |
|---|---|
| 分档审计 | `hi44` 104 片、`mid24` 113 片、`mono32` 150 片，全部通过 |
| 滤波器长度变化 | 104/104 片相位余量改变，传播链从 `[0]` 延伸到 104 片 |
| 静音区音频复用 | 一次 filter 变更中 35 片音频硬链接复用，113 片侧信道重写 |
| 侧信道陈旧 | 首个漂移片 20，`stale_side=sidecar` |
| 缓冲陈旧 | 首个漂移片 20，`stale_side=buffer`，512 样本、981 字节偏差 |
| 强杀恢复 | SIGKILL 返回 `-9`，强杀前 6 片已提交，恢复从第 6 片继续，inode 不变 |
| 发布隔离 | 并发替换期间数百次查询，0 次错误或混配 |
| 确定性 | 相同种子与操作序列的轨迹、发布清单逐字节一致 |

## 核心契约

### WAV 输入

仅接受 48000 Hz、16/24-bit 整数 PCM、1 或 6 声道。`WAVE_FORMAT_EXTENSIBLE`
声道掩码要求 mono=`0x00000004`、5.1=`0x0000003f`。以下矛盾均返回
`E_WAV_INVALID` 并给出字段：

- `fmt.formatTag`、`fmt.cbSize`、`fmt.subFormat`；
- `fmt.channels`、`fmt.sampleRate`、`fmt.bitsPerSample`、`fmt.validBitsPerSample`；
- `fmt.blockAlign`、`fmt.byteRate`、`fmt.dwChannelMask`；
- `data.size` 不是整帧长度、chunk 越界、重复/缺失 chunk。

### 重采样、切片与相位

对 48 kHz 源与目标率 `target_rate`，约分：

```text
step_num / step_den = 48000 / target_rate（约分）
source_position(j) = j * step_num / step_den
```

每个输出样本使用长度为 `L` 的 Blackman 窗 sinc 内核，支持区间为
`center-L/2+1 ... center+L/2`。只在整文件边界截断，不在切片边界重置支持区间，
因此下一片自然读取上一片尾部重叠的滤波器历史。

侧信道相位定义为：

```text
phase_num = (out_start * step_num) mod step_den
end_phase_num = (out_end * step_num) mod step_den
phase_remainder_num =
    (phase_num + (filter_length // 2) * step_num) mod step_den
```

下一片的 `phase_num` 必须等于上一片 `end_phase_num`；下一片
`phase_remainder_num` 由上一片结束相位加半滤波器步进得到。滤波器长度改变会改变
该余量，因此不能只按切片参数依赖判断。

每片记录：

```text
index, out_start/out_end, src_start/src_end, phase_num/phase_den,
phase_remainder_num, end_phase_num, peak_abs, per_channel_peaks,
silent, content_hash, prev_chain_hash, chain_hash
```

`content_hash` 严格等于该片已提交 PCM 字节的 SHA-256；峰值和静音标记同样从
解码后的已提交样本计算，不使用估算缓存作为真值。

### 参数依赖与传播

参数版本由 `TierCatalog.configure()` 维护：参数不变时版本不变，任一参数变化时
版本加 1。变更后先沿旧切片逐片确定性重算，比较缓冲哈希与侧信道元数据，得到可
证明完全一致的前缀。

- 前缀切片可直接复用音频和侧信道；
- 从首个不一致切片开始，后续所有切片保守重写侧信道；
- 音频字节若与新真值完全一致，则用硬链接复用旧不可变缓冲；
- 首片原因记为 `param:<field>`，后续记为 `phase_propagation` 或
  `hash_chain_propagation`；
- 轨迹记录每片 `propagation_chain`、相位余量、复用/重算计数和内容哈希。

因此“音频可复用”和“侧信道必须重算”是分开记录的，不会把新缓冲与旧相位基准
混在一起。

### 错误码

| 错误码 | 含义 |
|---|---|
| `E_WAV_INVALID` | WAV 头、位深、布局、长度矛盾，带 `field` |
| `E_SIDECAR_DRIFT` | 侧信道/缓冲漂移，带首个漂移片、`stale_side`、偏差样本/字节数和逐片清单 |
| `E_VERSION_STALE` | 发布清单/版本哈希过期或被篡改 |
| `E_RECOVERY_INVALID` | 快照、链、样本级前缀或相位衔接无效，带问题切片 |
| `E_BUSY` | 同档位已有进程/线程在转码 |

## 存储与原子提交

```text
<store>/
  catalog.json                  # 档位参数与单调版本
  published.json                # 原子切换的发布清单
  trace.jsonl                   # 确定性轨迹
  runs/<tier>/<run_id>/
    snapshot.json               # 版本、参数、源哈希、总切片、依赖计划
    slices/000000/
      audio.pcm                 # 已提交 PCM 缓冲
      meta.json                 # 该片侧信道、相位、哈希链
    sidecar.json                # 聚合侧信道
    version.json                # 档位版本描述
    .complete                   # 最后写入的完成标记
```

每个切片先写入同目录临时目录，写入并 `fsync` 两个文件后整目录
`os.replace`，最后 `fsync` 父目录；恢复时只识别六位数字切片目录，忽略临时目录。
快照与清单均使用临时文件 + `fsync` + 原子重命名。

`published.json` 包含 `generation`、由全部档位描述重算的
`generation_hash`，以及每档 `tier_version_hash`、侧信道摘要与参数版本。
查询只读取一次清单快照，然后访问该清单指向的不可变 run 目录；因此替换期间只能
看到完整旧版或完整新版。旧 run 目录保留，不会被覆盖。

## 强杀恢复

1. 非阻塞 `flock` 保证同档位仅一个进程，占用时返回 `E_BUSY`；
2. 校验 `snapshot_version`、源文件哈希、档位参数、输出长度和切片总数；
3. 从 0 扫描连续已提交切片，逐片验证 PCM 哈希、侧信道元数据、样本级重算结果
   和 `prev_chain_hash -> chain_hash`；
4. 出现缺口、越界或断链时以 `E_RECOVERY_INVALID` 定位切片并中止；
5. 验证下一个未提交切片的 `phase_num` 与前片 `end_phase_num`、相位余量衔接；
6. 仅从首个未提交索引续写，不重写已验证前缀；
7. 全部切片完成后重新执行样本级审计，才写聚合侧信道和完成标记；
8. 最后原子替换发布清单。

## GUI

`live_transcode/gui.py` 提供：

- 三个可编辑档位标签页：目标率、滤波器长度、量化位深、切片样本数；
- 每档切片网格，颜色区分重算、复用、恢复前缀、取消和漂移；
- 恢复扫描进度、协作式取消、启动/续转、一致性校验；
- 同档位重复启动显示 `E_BUSY`；
- 漂移片标红并在提示中显示 `stale_side` 与偏差样本数；
- 本地样本生成与 WAV 加载。

## 模块

| 文件 | 职责 |
|---|---|
| `wavio.py` | RIFF/WAVE 手工解析、字段校验、16/24-bit 解码 |
| `models.py` | WAV/档位/比例模型、规范 JSON 与哈希 |
| `resample.py` | 确定性窗 sinc 重采样、量化、切片、内容哈希 |
| `sidecar.py` | 侧信道构造、版本哈希、逐片一致性审计与陈旧归因 |
| `dependency.py` | 参数依赖、复用前缀、强制传播计划 |
| `storage.py` | 档位目录、原子切片提交、清单、查询隔离 |
| `engine.py` | 生成/续转/发布/审计编排 |
| `samples.py` | 固定种子扫频与静音样本生成 |
| `trace.py` | 规范 JSONL 轨迹 |
| `gui.py` | PySide6 审看界面 |

## 说明与边界

- 这是本地确定性参考实现，不是实时播放器；目标是可验证的样本级接续和恢复语义。
- 重采样浮点运算固定为 NumPy `float64`，量化固定 `rint`，所有文件写入使用规范
  JSON 与固定顺序，因此相同输入和操作序列可逐字节复现。
- `filter_length` 要求为偶数，范围 8–1024；目标率 8–192 kHz；量化位深 16/24。
- 本实现只处理单声道与标准 5.1（FL/FR/FC/LFE/BL/BR）整数 PCM。
