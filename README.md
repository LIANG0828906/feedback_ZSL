# 本地无障碍描述轨节奏工作台

本项目把纪录片字幕语音句与无障碍描述句统一建模为整数样本时基上的、带优先级的阅读节奏问题。核心只依赖 Python 标准库，PyQt5 工作台与 NumPy 波形加速为可选本地依赖。

## 已实现能力

- 统一 WAV 样本、`HH:MM:SS.mmm` 时间码、自定义二进制帧表和句子阅读长度到 48 kHz 整数样本轴；错误定位到首个句子或 `frame_table`。
- 语音句与描述句共用可读下限、可用区间、优先级和显示时长模型；不可读句显式标记，不静默通过。
- 描述句冲突按优先级压缩语音、顺延语音或拒绝描述；重叠/间隔链迭代消解，达到上限时以 `E_DESC_CONFLICT` 返回链路、决策和检查点证据。
- 增量编辑从变更句沿旧冲突边、依赖边和可证明的时间覆盖范围求影响集合；未受影响句复用旧结果和哈希，无法证明时保守纳入并记录原因。
- 每条描述句的自身可读性、与每条语音句的重叠和最小间隔均在合规报告中逐项列出；违规项包含重建结论所需的区间、阈值、实际值和冲突对象。
- 每次提交生成完整 JSON 快照、版本哈希链、原子轨道/报告/轨迹文件。编辑 journal 支持强制终止后的确定性续跑。
- 后台线程求解与不可变快照整体发布；查询只能读到完整旧版本或完整新版本，旧句柄返回 `E_STALE_HANDLE`，同素材并发求解返回 `E_BUSY`。
- PyQt5 工作台显示波形、语音/描述轨、冲突高亮，支持编辑、后台求解、协作式取消、journal 恢复和固定 100 ms 预览刷新。
- 确定性样本生成器覆盖单声道稀疏、单声道密集冲突、5.1 密集冲突，并输出逐文件 SHA-256 基线。

## 运行

```bash
python3 --version                    # >= 3.10
python3 -m unittest discover -s tests -v
python3 -m a11y_rhythm demo --out artifacts/demo
```

GUI 环境安装：

```bash
python3 -m pip install --no-index --find-links ./wheelhouse -r requirements.txt
python3 -m a11y_rhythm gui \
  --source artifacts/demo/samples/mono_conflict.json \
  --store artifacts/demo/gui.session.json
```

当前题目容器是 Python 3.14.4，未提供 pip、NumPy 或 PyQt5。因此核心、CLI、样本、测试和验收演示均使用标准库执行；`gui` 在缺少 PyQt5 时会返回明确提示。项目没有访问任何外部网络，也不使用容器。

## CLI

```bash
# 生成 3 组确定性本地素材和 manifest 哈希
python3 -m a11y_rhythm generate --out artifacts/samples

# 校验 WAV/JSON/帧表时基
python3 -m a11y_rhythm verify-timebase --source artifacts/samples/mono_conflict.json

# 创建版本 1 和交付文件
python3 -m a11y_rhythm solve \
  --source artifacts/samples/mono_conflict.json \
  --store artifacts/work/conflict.session.json

# 修改描述文本，只重解受影响冲突链，发布版本 2
python3 -m a11y_rhythm edit \
  --source artifacts/samples/mono_conflict.json \
  --store artifacts/work/conflict.session.json \
  --item-id ds001 \
  --text "A longer recovered description."

# 若进程在编辑中终止，从 journal 继续未完成链
python3 -m a11y_rhythm recover \
  --source artifacts/samples/mono_conflict.json \
  --store artifacts/work/conflict.session.json
```

## 核心设计

**时基。** WAV 使用 RIFF 解析并接受 PCM/WAVE_FORMAT_EXTENSIBLE PCM；帧表是带 `FTB1` 魔数的 little-endian 格式。所有时间码先转为有理数秒，再四舍五入到整数样本。可读下限为 `ceil(text_length * sample_rate / max_reading_rate_cps)`。详细格式见 `docs/FORMATS.md`。

**求解。** 句子按原始起点、优先级和 ID 确定性排序。描述句初始至少占用其可读下限；描述与语音冲突时，语音优先级不低于描述则拒绝描述，否则尝试无损压缩语音，低于下限则顺延。语音与语音按时间顺序保持稳定，不因优先级交换位置，避免链振荡。每轮只处理排序后的首个冲突并记录证据，超过 `max_iterations` 返回 `E_DESC_CONFLICT`。

**局部性。** 编辑后先取旧冲突图的传递闭包，再用“起点 + 最大可能时长”的保守右边界扫描下游；起点在编辑句之前且无旧冲突边的句子可复用。若重解触及不可变句、缓存 fingerprint 不符或复用区间改变，影响集合扩张后重跑。最终 `reused_ids` 的逐句 `result_hash` 必须与旧版本一致。

**版本与恢复。** 已提交快照是唯一真相；编辑 journal 只是未提交检查点。快照链、payload、逐句结果、报告和轨道哈希均在加载时复算。恢复从检查点迭代号继续，而不是重做整个物料。快照原子替换成功后再删除 journal；若终止发生在两者之间，`txn_id` 可证明提交已完成。

**并发隔离。** `PublishService` 在锁内整体替换一个快照引用。读者在锁内取完整 payload 副本；发布后旧 `VersionHandle` 立即失效。工作线程只操作独立深拷贝/不可变句子模型，不读写界面对象。

## 错误契约

| 错误 | 场景 |
|---|---|
| `E_TIMEBASE_INVALID` | 时间码非法/越界、WAV 或帧表时基不符、帧表长度矛盾 |
| `E_DESC_CONFLICT` | 冲突链达到迭代上限或音频末端仍不可消解 |
| `E_RECOVERY_INVALID` | 快照/journal 断链、缺字段、版本或源哈希不符、检查点损坏 |
| `E_STALE_HANDLE` | 查询使用已被新版本替换的句柄 |
| `E_BUSY` | 同素材已有后台求解会话 |
| `E_CANCELED` | 协作式取消；未提交 journal 保留供恢复 |

## 验收证据

运行 `python3 -m a11y_rhythm demo --out artifacts/demo` 后：

- `artifacts/demo/verification.json`：冲突链、增量范围、强制终止恢复、确定性和交付文件索引。
- `artifacts/demo/delivery.json`：版本 2 的语音轨、描述轨、说话人标记、合规报告。
- `artifacts/demo/conflict.session.trace.jsonl`：逐事件轨迹，包含版本、句子 ID、受影响数、冲突链长度和内容哈希。
- `artifacts/demo/samples/manifest.json`：目录内 WAV、帧表、JSON 的 SHA-256。

本次实际验证：

- `python3 -m unittest discover -s tests -v`：26 项测试全部通过。
- 密度样本初始解：54 次确定性冲突消解，所有活动句可读，合规报告通过。
- 稀疏样本描述文本增长：仅重解 4/8 句，4 句复用哈希不变。
- 同一稀疏素材连续两次编辑：内存版本、磁盘版本均到 3，历史链 `[1,2,3]`，内存/磁盘链哈希一致。
- 编辑第 2 个检查点后终止，并有真实子进程 `os._exit(77)` 用例：journal 存在；恢复从第 2 轮继续；恢复版本链与无中断控制运行逐字节链哈希相同。
- 恢复后的版本 2 交付轨包含 12 条语音、5 条描述、17 个说话人标记，合规报告 0 违规。
- 同种子同操作序列：快照链、结果哈希和轨迹 JSONL 字节一致。

## 目录

```text
a11y_rhythm/
  timebase.py       WAV/时间码/帧表统一与校验
  solver.py         优先级节奏与冲突链、检查点
  compliance.py     逐句合规报告、轨道导出
  store.py          原子快照、哈希链、journal、恢复
  service.py        后台求解、整体发布、句柄隔离
  samples.py        确定性 WAV/帧表/JSON 生成器
  gui.py            PyQt5 工作台
  demo.py           可复现验收演示
  cli.py            命令行入口
tests/              26 项标准库单元/集成测试
docs/FORMATS.md     二进制和 JSON 精确格式
```
