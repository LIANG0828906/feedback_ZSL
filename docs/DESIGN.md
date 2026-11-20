# MixRec 技术设计

## 1. 模块结构

```text
mixrec/
  cli.py         命令行入口
  engine.py      调度、提交事务、增量发布、恢复
  errors.py      稳定错误码
  generator.py   确定性 WAV / 配置 / 基线生成器
  gui.py         PySide6 工作台
  index.py       索引、哈希链、封印和恢复校验
  mixer.py       多轨混流与内存估算
  models.py      配置模型、区间运算和参数指纹
  resample.py    三次多项式插值、声道转换、量化
  trace.py       JSONL 轨迹
  wavio.py       16-bit PCM WAV 解析与原子写入
```

引擎不导入 PySide6；无界面环境可只安装 NumPy。

## 2. 统一时基与混流

输出样本 `n` 映射到轨道源位置：

```text
source_position = (n - delay_samples) * source_rate / output_rate
```

整数部分选择四个局部邻居，使用 Catmull-Rom 三次多项式：

```text
y(t) = y1 + 0.5t[(y2-y0) + t(2y0-5y1+4y2-y3 + t(3(y1-y2)+y3-y0))]
```

每个输出位置只依赖全局样本编号和四个源样本，因此：

- 整轨渲染与任意块大小渲染逐字节一致；
- 切片从任意边界开始渲染，不需要跨切片滤波器状态；
- 16 kHz ↔ 48 kHz 重采样不会在切片边界重置状态或产生重复 / 丢失样本。

静音区间按输出样本计算：

```text
[floor(start_seconds * output_rate) + delay,
 floor(end_seconds  * output_rate) + delay)
```

声道规则为单声道复制到双声道、双声道取平均到单声道。各轨应用线性增益后累加到 float64 缓冲，整体限幅到 `[-1, 1]`，使用 `rint` 量化为 16-bit PCM。

## 3. 切片连续性与内容标识

切片 `i` 的边界为：

```text
start = i * slice_samples
end   = min((i + 1) * slice_samples, total_samples)
```

提交前检查：

1. `start_sample` 等于前一已提交切片的 `end_sample`；
2. `sample_count == end_sample - start_sample`；
3. 实际渲染数组长度等于 `sample_count`；
4. 索引恢复时逐片重算边界。

`content_sha256` 对交错 little-endian int16 PCM 计算；`file_sha256` 对完整 WAV 文件计算。索引校验同时读取 WAV 并重算两个哈希。

## 4. 索引、哈希链与版本

每个切片记录的链哈希包含：

```text
genesis, prev_chain, index, start, end, sample_count,
content_sha256, file_sha256, file, artifact_version, parameter_versions
```

`genesis_chain` 只由作业 ID、输出契约和索引 schema 决定。因此增益 / 静音修改会扩展同一条录制链，而不是创建新链。

`index_seal_sha256` 额外封印：

```text
schema, job_id, index_version, chain_head, genesis_chain,
params_fingerprint, committed_slices, total_slices, track_versions
```

它让 `index_version` 或轨道版本表被篡改时也能在恢复阶段返回 `E_RECOVERY_INVALID`。旧版无封印索引仍可读取，但本项目生成的新索引均带封印。

版本语义：

- `index_version`：每次索引原子发布加一。初始逐片提交得到 `1..N`；增量发布只加一。
- `artifact_version`：切片文件目录代次。初始为轨道参数版本最大值；增量取旧产物代次加一，避免覆盖旧索引仍引用的文件。
- `parameter_versions`：该切片计算时每个轨道的参数版本。

## 5. 内存预算与调度

单切片估算包括：

- float64 混音累加缓冲；
- int16 量化缓冲与 WAV 编码缓冲；
- 各轨源样本 gather 缓冲；
- 固定 Python / WAV 临时空间。

估算按 16 字节对齐。若超过 `per_slice_budget_bytes`，内部 `block_samples` 依次减半，最低到 128，再重新估算。仍超限则任务不启动并返回 `E_BUDGET_EXCEEDED`。

调度器按切片序号生成候选批次：

1. 受 `max_workers` 和全局 `memory_budget_bytes` 限制；
2. 放不下的任务写 `task_waiting`，留到后续批次；
3. 已预留任务并发执行；
4. 完成结果按切片序号排序后提交。

实际观测到的完成顺序不进入提交决策。`task_priorities` 只出现在轨迹中，不会改变批次或提交顺序，从而保证高优先级任务不能插队破坏录制顺序。

## 6. 原子提交与失败隔离

初始运行每片事务：

1. 在切片目录写临时 WAV，`fsync`；
2. `os.replace` 到正式切片路径；
3. 追加切片记录、重算链和封印；
4. 索引写临时 JSON，`fsync` 后 `os.replace`；
5. 输出 `slice_committed`。

因此强制终止后，`index.json` 总是指向一个连续、完整且哈希可验证的前缀。临时 WAV 或临时索引不会被识别为已提交产物。

同批次中若第 `k` 片渲染或长度校验失败：

- `0..k-1` 可以按序提交；
- 第 `k` 片及后续不写正式文件；
- 索引保持前缀，进程退出后从 `k` 继续。

预算拒绝发生在任务启动前，因此不会产生该切片文件或临时正式路径。

## 7. 增量重算

修改某一轨后：

- 增益或延迟：影响全部切片，采样区间为整个时间线；
- 静音区间：对旧区间集合和新区间集合做对称差，例如
  `[[18240,24960]]` 改为 `[[19200,21120]]` 且延迟为 31 时，变化区间为
  `[[18271,19231],[21151,24991]]`；
- 未变化参数：空集合，只发布新参数版本。

对每个受影响切片，使用新参数重新渲染到新的产物代次目录。未受影响切片原样复用文件、内容哈希、文件哈希和参数版本。

全部临时 WAV 完成后：

1. 以原子重命名发布新切片文件；
2. 从头重建整条链（未受影响内容仍复用）；
3. 一次性原子替换索引；
4. 更新配置快照。

读取方通过原子替换只会看到完整旧索引或完整新索引；新文件代次与旧索引引用的文件不重叠。

## 8. 强制终止恢复

恢复步骤：

1. 读取索引并检查 schema、边界、连续前缀；
2. 校验每条链、链头和索引封印；
3. 校验参数指纹、轨道版本和来源 WAV 哈希；
4. 逐片校验 WAV 文件哈希和 PCM 内容哈希；
5. 从 `committed_slices` 对应样本位置继续。

任何校验失败返回 `E_RECOVERY_INVALID`，发生在任何切片写入之前，因此不会覆盖已有产物。若配置快照与索引参数不同，索引中的轨道参数是权威状态，可从索引恢复后继续。

## 9. 确定性

确定性来自以下约束：

- 固定种子的 NumPy `PCG64` 生成器；
- 固定 JSON 键排序与紧凑分隔符；
- 固定 WAV 头和 little-endian PCM；
- 不使用局部时间戳、线程 ID、随机临时文件名或绝对路径；
- 并行结果按切片序号排序后提交；
- 轨迹写入顺序只由协调线程决定。

相同种子、配置和操作序列下，样本 WAV、切片 WAV、索引和 JSONL 轨迹逐字节一致。

## 10. 验证矩阵

自动测试覆盖：

- 生成器跨目录逐字节确定性；
- 16 kHz / 48 kHz、单 / 双声道读取和声道转换；
- 整轨渲染与不同块大小渲染一致；
- 切片拼接 PCM 与整体混流一致；
- 延迟越界、声明声道不符的 `E_MIX_INPUT` 与轨道定位；
- 内存预算降级后的拒绝和无正式产物；
- `E_BUSY` 重复启动；
- 增量只重算 4/5/6、复用 12 片；
- 增量预算失败保持旧索引字节不变；
- 从第 3 片前缀恢复后内容哈希和链头一致；
- 损坏切片返回 `E_RECOVERY_INVALID` 且不覆盖索引；
- 索引边界缺口定位到切片序号；
- 高优先级任务不改变提交顺序；
- 相同操作下切片树和轨迹逐字节一致。

实际运行结果记录在 `artifacts/validation/summary.json` 和本仓库根目录 README 中。
