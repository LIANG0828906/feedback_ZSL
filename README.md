# HDR Delivery Studio

面向后期制作的本地 HDR/SDR 统一交付工作台。项目使用 Python 3.11、NumPy 与 PySide6，
实现了确定性 HDR 帧序列、float64 色彩转换链、增量版本发布、非法参数保护、原子快照恢复
以及可交互时间轴预览。

## 快速开始

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt

.venv/bin/python -m pytest
.venv/bin/python -m hdr_studio.cli demo --workspace ./work/demo --force
.venv/bin/python -m hdr_studio.cli workbench --workspace ./work/demo
```

无图形会话时使用脚本化入口，或对 Qt 做离屏冒烟测试：

```bash
QT_QPA_PLATFORM=offscreen HDR_STUDIO_AUTOCLOSE_MS=800 \
  .venv/bin/python -m hdr_studio.cli workbench --workspace ./work/demo --offscreen
```

完整可重复验收：

```bash
./scripts/verify.sh
```

## 已实现能力

- 自定义二进制 HDR 序列头：分辨率、帧数、位深、色彩空间、传递函数、峰值亮度、
  帧长度与 CRC32；任何头部缺失、未知码、长度不符或样本越界均返回 `E_HDR_INVALID`。
- Rec.709、Rec.2020、Display-P3 间转换；线性化、基色矩阵、Bradford 白点适应、
  目标传递函数编码均使用 float64。
- SDR 2.4 gamma、SMPTE ST 2084 PQ、BT.2100 HLG；`none`、`reinhard`、`hable`
  三种峰值映射；逐段曝光调整。
- 每个版本记录参数、受影响区间、逐帧哈希、内容哈希、中间链摘要与逐帧最大码值偏差。
- 版本差异、历史回退；回退生成新版本号，不覆盖旧记录。
- 非法参数在落盘前拒绝，旧快照、旧版本目录和旧哈希保持不变。
- 增量帧复用、staging 目录完整性复验、目录原子提升、快照临时文件加原子重命名。
- 强终止恢复：恢复最后已发布快照，丢弃未发布孤儿版本，仅重算缺失帧；
  损坏帧、断链或字段缺失返回 `E_RECOVERY_INVALID`。
- Qt 后台转换线程、忙状态保护、取消与播放预览；查询始终看到完整旧版本或完整新版本。
- 固定种子样本生成器与无时间戳、无文件大小的逐字节确定性轨迹日志。

详细格式、事务状态机和验收数据见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 工作区布局

```text
<workspace>/
  samples/                     # 生成的 .hdrseq 与 manifest.json
  versions/v000001/            # 不可变版本目录
    manifest.json
    frames/frame_000000.bin
  snapshots/current.json       # 最新有效发布快照（原子替换）
  snapshots/snapshot-v000001.json
  trace/trace.jsonl            # 确定性版本/拒绝/取消轨迹
  baselines.json               # demo 基线版本哈希
```

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_HDR_INVALID` | HDR 头、长度、位深、未知传递函数或样本越界 |
| `E_PARAM_CONFLICT` | 峰值/曲线无法压缩，或曝光使码值超出目标位深 |
| `E_BUSY` | 同一会话已有转换任务 |
| `E_VERSION_MISMATCH` | 发布前内容哈希不一致或版本目录冲突 |
| `E_RECOVERY_INVALID` | 快照断链、字段缺失、源不匹配或发布帧损坏 |
| `E_CANCELLED` | 可处理的任务取消，不改变最后发布版本 |

## Python API 示例

```python
from pathlib import Path
from hdr_studio.model import ColorSpace, ConversionParams, ToneMap, TransferFunction
from hdr_studio.session import ConversionSession

session = ConversionSession.open(
    Path("work/demo"),
    Path("work/demo/samples/rec2020_pq_10bit.hdrseq"),
)
record = session.submit(
    ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
    )
)
print(record.version, record.output_hash)
session.close()
```

