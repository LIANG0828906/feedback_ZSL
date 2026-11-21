# 游戏配音响度归档工作台

一个纯本地、可复现的 Python 工作台，把多声道 WAV 读取、v1/v2/v3 响度元数据解析、边界推导、分段整备、返工判定、版本升降级、跨版本复核、并行调度、原子归档、强杀恢复和不可变版本发布串成一条可追溯链路。

- 音频：48 kHz、16-bit/24-bit PCM、单声道/5.1
- 元数据：严格 JSON schema，v1/v2/v3 统一内部表示
- 计算：BS.1770-4 K 加权短期响度峰值、4 倍插值真峰值
- 归档：每段临时文件 + `os.replace`，清单哈希链，按段增量恢复
- 发布：完整快照构建后原子替换指针，查询不会看到半更新结果
- 界面：Tk 分段网格、返工标记、字段映射、恢复进度、轨迹日志
- 验证：16 项自动化测试，包含真实子进程 `os._exit(137)` 强杀恢复

详细状态机、schema 和算法说明见 `docs/technical.md`。

## 环境准备

要求 Python 3.10+、NumPy、Tkinter。系统包可用时：

```bash
sudo apt-get install python3-numpy python3-tk
```

当前无 root 环境可用用户级脚本安装（会创建 `.venv`，并从 Ubuntu 包解包 Tk 到 `.vendor/`，不修改系统目录）：

```bash
./scripts/bootstrap_local.sh
source scripts/env.sh
```

之后统一使用 `$LA_PYTHON`：

```bash
"$LA_PYTHON" --version
"$LA_PYTHON" -c 'import numpy, tkinter; print(numpy.__version__, tkinter.TkVersion)'
```

## 快速验收

```bash
bash scripts/verify.sh
```

该脚本会：

1. 生成 4 个确定性 WAV 样本及每个样本的 v1/v2/v3 元数据；
2. 运行 16 项测试；
3. 输出基线、复核报告和轨迹日志的 SHA-256。

只生成样本：

```bash
source scripts/env.sh
"$LA_PYTHON" -m loudness_archive generate \
  --out artifacts/generated/demo \
  --seed 20261122 \
  --summary
```

打开工作台：

```bash
./scripts/gui.sh --workspace workbench_data
```

工作台可载入 WAV 与 v1/v2/v3 元数据，切换目标版本，预览逐段值、超限标记、字段映射和损失清单；归档按钮会自动识别已有清单并从首个未提交段恢复，Cancel 为协作式取消。

## 元数据格式

所有版本共有字段：

```json
{
  "version": 1,
  "asset_id": "wav-012345...",
  "sample_rate": 48000,
  "duration_samples": 120000,
  "integrated_lufs": -23.4,
  "target_offset_lu": -0.4
}
```

### v1

只有整轨积分响度和目标偏差，没有声道布局、分段边界、逐段指标或校验码。

### v2

追加：

```json
{
  "segment_count": 4,
  "true_peak_limit_dbtp": -1.0,
  "segments": [
    {
      "index": 0,
      "short_term_lufs": -20.1,
      "true_peak_dbtp": -3.2,
      "over_limit": false
    }
  ]
}
```

v2 不含边界表，按 `start_i=floor(i*duration/count)` 规则推导。

### v3

追加：

- `channel_layout`、`channel_mask`
- `downmix_weights`，顺序与 WAV 声道顺序一致
- 连续覆盖整轨的 `segments[].start_sample/end_sample`
- 每段 `true_peak_limit_dbtp`、`excess_db`
- `quantization_tolerance_db`
- `archive_checksum`

校验码：

```text
sha256(canonical-json:v1, excluding archive_checksum)
```

字段缺失、版本未知、边界断裂、声道掩码/布局/权重不一致或校验码不符时返回 `E_META_INVALID`，并给出首个异常字段路径。解析阶段不会用默认值静默补齐。

## 升降级规则

- v1/v2 → v3：保留整轨字段；从 WAV 头严格取得布局和掩码；从布局推导下混权重；v2 使用全局真峰值上限，v1 使用显式配置策略；重算逐段值；生成 v3 校验码。
- 每次推导写入字段映射表，包括决策号、字段、动作、来源、算法、输入和结果。
- v3 → v1/v2：默认抛出 `E_VERSION_LOSS`，列出受影响段号、逐段字段和全局字段。调用方必须显式允许有损输出。
- 复核报告保留声明值、重算值、有效值、来源、限值和容差，可仅凭报告字段复现结论。

## CLI 示例

设：

```bash
DATA=artifacts/generated/demo
WAV=$(find "$DATA/samples" -name '*.wav' | head -1)
META=$(find "$DATA/metadata" -name '*.v3.json' | head -1)
```

解析与升级：

```bash
"$LA_PYTHON" -m loudness_archive inspect --wav "$WAV" --metadata "$META"
"$LA_PYTHON" -m loudness_archive upgrade --wav "$WAV" --metadata "$META" \
  --out /tmp/upgraded.v3.json
```

有损降级必须显式允许，损失清单仍完整输出：

```bash
"$LA_PYTHON" -m loudness_archive downgrade --wav "$WAV" --metadata "$META" \
  --target v1 --allow-loss --out /tmp/downgraded.v1.json
```

跨版本复核：

```bash
V1="${META%.v3.json}.v1.json"
V2="${META%.v3.json}.v2.json"
"$LA_PYTHON" -m loudness_archive review \
  --wav "$WAV" --v1 "$V1" --v2 "$V2" --v3 "$META" \
  --out artifacts/generated/review.json
```

归档、强杀后恢复、查询：

```bash
WORK=artifacts/generated/archive
# 下一个命令预期以 137 退出，模拟进程被强制终止。
"$LA_PYTHON" -m loudness_archive archive \
  --workspace "$WORK" --wav "$WAV" --metadata "$META" \
  --crash-after 2 --no-publish

"$LA_PYTHON" -m loudness_archive recover \
  --workspace "$WORK" --wav "$WAV" --metadata "$META"

"$LA_PYTHON" -m loudness_archive query \
  --workspace "$WORK" --version-id ver-000001
```

如果重复使用同一个工作区，需先清理 `$WORK`，否则已有提交会被跳过，不一定能再次触发“前 2 段后终止”的演示。

## 项目结构

```text
loudness_archive/
  wav.py          RIFF/WAVE 解析、16/24-bit PCM、掩码与长度校验
  loudness.py     K 加权、短期响度、真峰值、逐段分析
  metadata.py     三版 schema、统一表示、边界推导与差异
  versions.py     升级、降级、字段映射、损失清单
  review.py       跨版本逐段复核与可复现报告
  scheduler.py    稳定排序、并行度、内存预算与等待队列
  archive.py      原子分段提交、哈希链、文件锁、强杀恢复
  publish.py      不可变快照、原子发布、查询与 stale 检查
  generator.py    确定性 WAV/元数据/基线/轨迹生成器
  gui.py          Tk 工作台
  cli.py          命令行入口
config/workbench.json
scripts/bootstrap_local.sh
scripts/env.sh
scripts/verify.sh
docs/technical.md
tests/
```

## 实际验证结果

验证环境：Python 3.14.4、NumPy 2.5.5、Tk 8.6。

```text
Ran 16 tests in 4.288s
OK
```

覆盖项包括：

- 16/24-bit、单声道/5.1 WAV 及错误声道掩码；
- 三版解析、首个异常字段、v3 校验码篡改；
- v1/v2 边界推导、升级映射、降级损失清单；
- 跨版本声明值/重算值差异、首个不一致段和字段；
- 调度稳定顺序、内存等待、`E_BUSY` 和超预算；
- 模拟强杀恢复不重写已提交段；
- 真实 CLI 子进程 `os._exit(137)` 后恢复 2→5 段；
- 链断和段文件截断定位首个异常段；
- 发布查询、错误版本哈希、清单链头过期；
- 并发查询只观察到完整旧/新快照；
- 同种子两次生成的全部文件逐字节一致；
- Tk 工作台可构造。

种子 `20261122` 的最终验收产物：

```text
baseline.json       6c96824497d7dc46f7b42559c0bb2704f2e5d4af6a774bede68a653e04bba729
review-report.json c90d621676929668a24d484d291c4d626e1cd5904a89ab87d127cf34aa13528d
trace.jsonl        7e6616497148e1dcb547382e75ccb57fa29e4ef8e280bb651506e64caf370e5e
```

恢复演示结果：提交段 `[0, 1]`，恢复前清单版本 `2`，恢复后清单版本 `5`，只补算 `[2, 3, 4]`，发布版本 `ver-000001` 的查询哈希与发布哈希一致。

## 设计边界

- 短时窗口默认 3 秒；若段更短，则窗口等于段长。生成器使用 0.5 秒分段，因此段内短期响度取整段 K 加权值。
- 真峰值是确定性的 4 倍 Catmull-Rom 过采样近似，不是平台相关的外部 True Peak 插件。算法版本写入配置、轨迹和复核复现区。
- v1 没有真峰值上限，升级时必须使用显式配置策略，并在字段映射中标记 `policy`。
- v2 没有边界表，其块值不能被视为显式边界声明；升级/复核会按声明公式推导并保留原声明值用于审计。
- 工作台一次处理一个素材；批量调度器作为独立模块提供稳定排队和全局预算控制。
