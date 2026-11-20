# 科学成像序列处理工作台

面向 16-bit 单色图像序列的确定性处理与诊断工作台。项目提供自定义二进制格式、定点/双精度双路径、逐帧逐像素误差上界、不可信像素归因、影响因子增量重算、按块哈希链快照恢复、PyQt5 诊断界面和脚本化样本入口。

## 快速开始

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install numpy PyQt5 pytest

# 生成 3 组不同分辨率、帧数、坏点密度和噪声水平的样本
python -m sciworkbench demo --out samples/demo

# 打开图形工作台并载入内置样本
python -m sciworkbench gui --demo

# 运行验收测试
pytest -q
```

核心处理不依赖 Qt；`PyQt5` 仅在启动 `gui` 子命令时导入。固定输入、参数和模式下的输出帧、误差摘要与轨迹日志均为确定性内容，不含时间戳，也不以文件大小代替 SHA-256 真值。

## 自定义序列格式

固定头使用小端 `<8sHHIIIHHQQII`，共 52 字节；`header_size` 默认为 64，剩余字节为 0。帧数据从 `data_offset` 开始，每帧占用 `frame_stride` 字节。

| 偏移 | 字段 | 类型 | 说明 |
|---:|---|---|---|
| 0 | `magic` | 8 bytes | `SCIIMG01` |
| 8 | `version` | u16 | 格式版本，当前为 1 |
| 10 | `header_size` | u16 | 头部总长度 |
| 12 | `width` | u32 | 宽 |
| 16 | `height` | u32 | 高 |
| 20 | `frame_count` | u32 | 帧数 |
| 24 | `bit_depth` | u16 | 必须为 16 |
| 26 | `pixel_format` | u16 | 1=`MONO16_LE`, 2=`MONO16_BE` |
| 28 | `data_offset` | u64 | 首帧偏移 |
| 36 | `frame_stride` | u64 | 每帧字节数 |
| 44 | `flags` | u32 | 保留标志 |
| 48 | `reserved` | u32 | 保留 |

解析器校验魔数、版本、头长、宽高帧数、位深、像素格式、帧跨度、数据偏移和精确文件长度。截断时计算第一个不完整帧；字段矛盾、未知格式或长度不一致返回 `E_IMG_INVALID`，并在错误对象中给出 `first_bad_frame`。

`SequenceStructure` 同时建立帧索引和分块索引。每个 `BlockRef` 携带全局序号、帧号、块号、像素边界和形状；分块大小独立于文件格式，尾部块允许非方形。

## 处理算子与误差链

每块在内部带 2 像素光晕计算，4 邻域以上信息足以让降噪阶段读取相邻块边界，块外结果不会就地改写。算子顺序固定：

1. 暗场减除：`v1 = raw - dark`
2. 平场除法：`v2 = v1 / max(flat, flat_floor)`
3. 坏点修复：坏点取有效 8 邻域均值，无有效邻居时退化为可用邻域均值
4. 分块增益：控制网格双线性插值，`v4 = v3 * gain`
5. 噪声抑制：3x3 高斯核滤波，`sigma=0` 时退化为恒等核

固定模式下，每步结果按 `2^-frac_bits` 网格、半远离零规则量化；浮点模式不量化像素，但执行完全相同的算子语义。固定滤波系数另行量化到 `coeff_bits`。

误差传播不是单一标量，而是五节点贡献向量。最终每像素上界为五节点贡献之和：

- 暗场：`dark_error + q/2`
- 平场：已有贡献除以分母下界 `max(flat_floor, max(|flat|,floor)*(1-rel))`，加上 `|v1|*rel/denom_lower + q/2`
- 坏点：邻域贡献均值，加邻域值跨度的一半与 `q/2`
- 增益：已有贡献乘 `|gain|`，加 `|v3|*gain_interp_error + q/2`
- 降噪：贡献按 `|kernel|` 卷积，加 `sum(|v|*|kernel_fixed-kernel_float|) + q/2`

其中增益插值误差使用局部单元曲率上界 `(|d2x|+|d2y|)/8`。每步每帧输出 `max_abs_error`、`max_error_bound`、`sum_error_bound` 与均值。定点结果和浮点参考逐块同时计算，结果对象记录最终逐像素偏差统计。

像素上界超过 `ErrorModel.threshold` 时标记为不可信，并归因到贡献最大的节点（0..4）。贡献非有限或没有正贡献节点时返回 `E_ERROR_ATTR`，错误位置包含帧、块和像素。

## 增量重算

影响因子为：

- `dark`
- `flat`
- `bad_mask`
- `block_gain`
- `noise`
- `error`
- `frac_bits`
- 精度模式

修改后按内容比较新旧因子，不使用时间或修改标记猜测。暗场、平场和坏点表只失效发生变化的帧与像素所在块，并扩展 1 个块的光晕；分块增益按实际采样值和局部插值误差变化确定失效块；噪声、误差模型、小数位或模式变化失效全部块。其余块直接复用缓存像素、误差和 `content_hash`。

处理期间旧 `ProcessResult` 保持可读；全部块完成后在锁内一次性替换为新的内存版本。未变化块不重新进入数值流水线。

## 快照与恢复

每个序列使用内容哈希隔离快照目录，每次处理建立 `vNNNNNN` 版本：

- `manifest.json` 先经临时文件加原子重命名发布，记录序列/头部/模式/影响因子版本和完整块顺序。
- 每块写入 `blocks/NNNNNN.blk`，内容包括像素、误差上界、归因、步骤摘要、影响因子版本、前一块哈希和本块 SHA-256。
- 块文件先写同目录临时文件，`fsync` 后 `os.replace`，并同步目录。
- 全部块提交后写 `complete.json`，封印最终链头。

恢复选择最新版本，验证清单、块字段、版本、内容哈希和前后链。合法的连续前缀直接视为已提交块，绝不重复写入；尾部缺失视为未完成并继续。中间缺块、字段缺失、内容变化、链头不一致或完整标记矛盾均返回 `E_RECOVERY_INVALID`，并定位 `first_bad_block/frame`。

## 并发、取消与发布

活跃任务按序列内容哈希注册。同一序列已有任务时，后续提交返回 `E_BUSY`。处理在块边界检查协作取消；取消或异常后不发布部分结果，最后已提交快照保留，序列忙标记在 `finally` 中释放。文件均通过上下文管理器打开，不持有跨操作句柄。

## 命令行

```bash
# 单样本及基线
python -m sciworkbench generate --out samples/micro --preset micro

# 样本矩阵
python -m sciworkbench demo --out samples/demo

# 结构检查
python -m sciworkbench inspect --sequence samples/micro/sequence.simg --block-size 16

# 定点处理，输出帧/误差/归因/不可信掩码/轨迹
python -m sciworkbench process \
  --sequence samples/micro/sequence.simg \
  --calibration samples/micro/factors.cal \
  --block-size 16 --output out/micro --snapshot-root snapshots

# 双模式逐像素偏差统计
python -m sciworkbench compare \
  --sequence samples/micro/sequence.simg \
  --calibration samples/micro/factors.cal \
  --block-size 16 --output out/mode-comparison.json

# 模拟强制终止后恢复
python -m sciworkbench process ... --snapshot-root snapshots --interrupt-after 5
python -m sciworkbench recover ... --snapshot-root snapshots
```

`--interrupt-after N` 在提交 N 个块后直接以退出码 137 终止进程，不运行后续清理和结果发布；`recover` 或 `process --resume` 从磁盘快照继续。

处理输出目录包含：

- `frames/frame_NNNNNN.f64`：小端 float64 像素
- `errors/frame_NNNNNN.f64`：逐像素误差上界
- `attribution/frame_NNNNNN.i8`：最大贡献节点
- `untrusted/frame_NNNNNN.u1`：不可信掩码
- `result.json`：模式、因子哈希、帧摘要、步骤摘要、模式对比
- `summary_diff.json`：新旧步骤误差摘要差异
- `trace.jsonl`：逐帧逐块影响因子、重算/复用块数、误差上界和内容哈希

## PyQt5 工作台

```bash
python -m sciworkbench gui --demo
```

界面提供：

- 帧时间轴和分块网格；点击块可选择局部增益控制点
- 校正结果、误差热力图、不可信像素三种显示层
- 精度模式、小数位、阈值、噪声 sigma、暗场偏置、平场比例和选中块增益编辑
- 后台 `QThread` 重算、进度显示和协作式取消
- 双模式逐像素偏差统计

重复提交由引擎返回 `E_BUSY`。取消或异常不覆盖最后一次成功发布的视图结果。

## 内置样本

| 名称 | 分辨率 | 帧数 | 坏点密度 | 噪声 sigma | 分块 | 小数位 |
|---|---:|---:|---:|---:|---:|---:|
| `micro_64x48x4` | 64x48 | 4 | 0.002 | 1.0 | 16 | 12 |
| `fast_128x96x3` | 128x96 | 3 | 0.008 | 2.5 | 24 | 11 |
| `wide_96x72x6` | 96x72 | 6 | 0.004 | 1.8 | 16 | 13 |

`generate`/`demo` 同时写出确定性 `sequence.simg`、`factors.cal`、`baseline_error_summary.json`、`trace.jsonl`、原始输出帧和 `mode_comparison.json`。相同种子与操作序列下，序列文件、校准文件、输出帧与轨迹日志逐字节一致。

## 代码入口

- `sciworkbench/model.py`：二进制格式、帧/块索引
- `sciworkbench/pipeline.py`：五级算子、量化、误差传播、双路径
- `sciworkbench/factors.py`：影响因子与确定性校准格式
- `sciworkbench/engine.py`：增量失效、忙状态、版本发布、模式对比
- `sciworkbench/snapshot.py`：原子块提交、哈希链、前缀恢复
- `sciworkbench/sample.py`：确定性样本与基线
- `sciworkbench/cli.py`：脚本化诊断入口
- `sciworkbench/gui.py`：PyQt5 工作台

实际验证结果见 `VALIDATION.md`。
