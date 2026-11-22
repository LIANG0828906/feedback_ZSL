# Lumen Filter Studio 技术说明

## 1. 数据流

```text
PNG
  │  Pillow verify + decode
  ▼
DecodedImage(uint8, mode, shape, content_hash)
  │
  ├─ denoise ──► CacheEntry ──► snapshot
  │                  │
  ├─ sharpen ◄───────┘
  │      │
  │      └──────────► CacheEntry ──► snapshot
  │                         │
  └─ local_contrast ◄───────┘
          │
          └────────────► 最终图像 + 差异热力图
```

阶段严格按 `STAGE_IDS` 顺序执行。`DependencyGraph` 同时保存显式依赖、
拓扑顺序与下游闭包，启动时校验“依赖必须早于消费者”。

## 2. 图像与算法

- 输入模式限定为 `L / LA / RGB / RGBA`，数组统一为 `uint8`。
- 2D 单通道数组在算法内部临时视为 `H×W×1`，输出恢复原布局。
- 所有阶段逐通道循环，不跨通道混合，因此通道互相独立。
- 去噪：反射边界均值滤波，按 `strength` 与原始通道混合。
- 锐化：USM，局部细节绝对值低于 `threshold` 时不增强。
- 局部对比度：分块直方图均衡（CLAHE 风格），每块按
  `clip_limit × area / 256` 裁剪并均匀重分配，使用双线性插值连接相邻
  块中心，再以 `blend` 与原通道混合。
- 输出统一 `clip + rint + uint8`，并强制 C-contiguous。

阶段输出形状或 dtype 变化会被 `_validate_stage_output` 拒绝为 `E_STAGE`。

## 3. 缓存模型

`CacheEntry` 保存：

- `stage`、`cache_key`、`upstream_key`；
- `input_hash`、`stage_version`、`params`；
- 只读 NumPy 输出数组与输出 SHA-256；
- 阶段耗时与提交时间。

缓存键：

```python
sha256(canonical_json({
    "input_hash": input_hash,
    "stage": stage_id,
    "params": params,
    "upstream_key": upstream_key,
    "stage_version": stage_version,
}))
```

输入哈希覆盖模式、dtype、shape 与像素字节。参数在进入缓存键前会按声明
类型规范化，浮点数四舍五入到 6 位小数，拒绝 NaN/Infinity。

### 失效边界

`set_pipeline_params()` 只比较参数，不重算像素：

1. 找出按阶段顺序最早发生变化的阶段；
2. 通过依赖图求该阶段的下游闭包；
3. 只删除闭包内缓存条目；
4. 上游 `CacheEntry` 对象原样保留；
5. 把失效阶段状态标为 `invalid`。

输入内容变化是特例：整个输入上下文改变，因此清空所有阶段并删除旧快照。

## 4. 快照

快照是单个 ZIP 文件：

```text
manifest.json
stages/denoise.npy
stages/sharpen.npy
stages/local_contrast.npy
```

`manifest.json` 记录输入哈希、图像元数据、参数、参数指纹、阶段版本、
合法阶段前缀、输出哈希/布局、上次运行统计与错误。

### 原子提交

1. 在同一目录创建隐藏临时文件；
2. 写入 manifest 与所有 `.npy`；
3. flush 并 `os.fsync`；
4. `os.replace(tmp, final)`；
5. POSIX 下 fsync 父目录；Windows 使用 `os.replace` 原子替换语义。

异常时删除临时文件，旧快照保持不变。测试中模拟 `os.replace` 失败，验证
旧文件字节未变且没有残留临时文件。

### 恢复校验

恢复必须同时满足：

- 输入内容哈希与模式一致；
- 全参数指纹一致；
- 阶段算法版本完全一致；
- 阶段列表是 `denoise → sharpen → local_contrast` 的合法前缀；
- 每个输出 hash、dtype、shape 正确；
- 上游键与重新计算的缓存键一致。

任一项不匹配都会整体作废。不存在“保留部分旧阶段再混用新阶段”的路径。

## 5. 调度、暂停、取消与失败

`PipelineSession.run()` 持有非阻塞运行锁，同一会话不允许并行运行。
`ResourceLimits.max_parallel` 被强制为 1，因为阶段存在严格依赖且共享单个
缓存；顺序执行从结构上保证提交顺序，`_commit_lock` 仍保护缓存与快照
提交临界区。

`RunControl.checkpoint()` 在阶段边界、通道边界和局部对比度分块循环中被
调用：

- 暂停：循环等待，不占用 CPU 忙转；
- 取消：抛出内部 `PipelineCancelled`；
- 取消不会提交当前未完成阶段，之前已完成阶段已逐个快照保存。

单阶段异常：

- 该阶段状态 `failed`，记录 `E_STAGE` 与阶段名；
- 不提交失败输出；
- 下游标记 `blocked`；
- 上游缓存保持；
- `retry()` 从空闲阶段继续，最终重新验算。

最终结果一致性审计使用独立空缓存重新执行全部三阶段，不读取主缓存，也
不写快照。按阶段顺序和 NumPy 行优先顺序查找首个不等像素，返回：

```json
{
  "code": "E_DIFF",
  "stage": "local_contrast",
  "x": 0,
  "y": 0,
  "channel": 0
}
```

## 6. 诊断字段

`RunDiagnostics` 区分：

- `path_type`：`full` / `incremental`；
- `source`：空缓存、会话缓存、完整快照、部分快照；
- 每阶段 `recomputes`、`hits`、`elapsed_sec`、`invalidated`、`cache_key`；
- `effective_cache_entries` 与 `full_stage_count`；
- `snapshot_writes`、`restored_from_snapshot`、
  `previous_snapshot_complete`；
- `max_parallelism = 1`；
- 强制终止恢复时明确写入“不对强制终止前的完整报告作任何声明”。

缓存有效条目数受三阶段字典上限约束，因此增量路径
`effective_cache_entries <= full_stage_count`。

## 7. UI 线程模型

- 主线程只负责 Qt 渲染和交互；
- `SessionWorker` 在 `QThread` 中运行处理或全量验算；
- 工作线程通过 Qt signal 推送阶段事件；
- 参数控件在任务运行期间禁用，暂停/取消保持可用；
- 取消/关闭窗口会触发 `RunControl.cancel()`，并等待工作线程退出；
- 预览、依赖图、缓存表与诊断视图只读展示会话状态；
- 已提交输出数组设置为只读，避免面板逻辑静默修改。

## 8. 资源边界

`ResourceLimits` 默认：

- `max_buffer_bytes = 256 MiB`；
- `working_set_factor = 12`；
- `max_dimension = 16384`；
- `max_parallel = 1`。

解码后与每阶段前都会按数组字节数估算工作集；超限返回 `E_MEMORY`。快照
单阶段输出与总文件读取也设置上限。

## 9. 可测试性

`PipelineSession` 可注入 `stage_runner`，用于：

- 注入单阶段异常；
- 注入首像素差异以验证 `E_DIFF`；
- 验证失败不污染缓存；
- 验证重试路径。

硬终止测试使用独立子进程在首阶段快照提交后调用 `os._exit(37)`，父进程
再以全新会话执行恢复，避免把普通异常或正常取消误当成“强制终止”。
