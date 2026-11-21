# 技术说明与验证记录

## 架构

包 `spectral_bake/`（纯本地，无容器、无网络、无真实分光设备）：

- `errors.py` — 稳定错误码：`E_SPECTRUM_INVALID`、`E_BUSY`、`E_BUDGET_EXCEEDED`、`E_ORDER_NONDETERMIN`、`E_RECOVERY_INVALID`，均携带结构化上下文（如波长采样点索引）。
- `config.py` — 本地 JSON 配置：并行度、单任务/全局内存配额、降级序列、统一波长网格、观察者、白点、色适应策略、目标色彩空间；并行度与配额只从文件读取，运行时不得静默提高。
- `spectrum.py` — 光谱材质导入与规范化（需求 1）：校验波长严格递增与反射率值域 [0,1]，越界/缺段/非单调返回 `E_SPECTRUM_INVALID` 并定位采样点（`sample_index` + `wavelength_nm`）；按统一网格线性重采样并记录归一化参考值（峰值）。sRGB / Display-P3 材质经传递函数解码为线性 RGB 进入同一链路。
- `pipeline.py` — 色彩管理链路（需求 2）：光谱归一化（k 使完美白 Y=100）→ CIE 1931 2° 观察者配色函数积分（解析高斯拟合）→ 白点适应（Bradford / CAT02 / none，可配置）→ XYZ→sRGB / Display-P3 矩阵变换 → sRGB 传递函数编码为 8-bit 码值。每一步中间量以 SHA-256 内容哈希标识并写入清单 `chain_step_hashes`；白点/目标空间变化时重建映射并输出 `max_code_delta_vs_previous`。
- `budget.py` — 内存计量与降级（需求 4）：按波长采样点（8 B/点）、CMF 积分缓冲（3x8 B/点）、球面采样缓冲（sphere x batch x 8 B）、预览缓冲（res^2 x 16 B）计量；超单任务配额时严格按声明序列 `reduce_sphere_samples -> shrink_internal_batch -> reduce_preview_resolution` 逐级重试并记录每级用量；序列耗尽抛 `E_BUDGET_EXCEEDED`，已发布批次不受影响。
- `scheduler.py` — 并行调度器（需求 3、5）：任务按提交序号稳定准入，累计占用超全局配额进入等待队列（条件变量 FIFO），决策写入轨迹；同批次重复启动返回 `E_BUSY`；结果按固定材质序号归约，与线程完成次序无关，校验失败以 `E_ORDER_NONDETERMIN` 中止。
- `incremental.py` — 增量重烘焙（需求 6）：参数版本 = 全局参数哈希（光源/白点/观察者/色适应/目标空间）+ 逐材质参数哈希；全局变化则全部重烘焙，单材质变化仅该材质；无法证明未受影响（无先前批次、溯源缺失）则保守全量并标注原因；一次整体替换发布。
- `batch.py` — 原子提交与恢复（需求 7）：清单写临时文件 + fsync + `os.replace` 原子重命名；哈希链 `chain[i]=sha256(chain[i-1]|entry_hash)`，校验时从码值重算条目内容哈希；逐球 journal 供强制终止后复用已提交球；断链/字段缺失/条目哈希不符则 `E_RECOVERY_INVALID`。
- `engine.py` — 编排：恢复最近有效批次，计算受影响集合，降级适配，调度烘焙，固定序号装配，原子发布；轨迹 JSONL 含材质序号、参数版本、受影响/复用球数、配额用量、降级阶段、内容哈希。
- `samplegen.py` — 确定性样本生成器（需求 8，固定种子 20261122）：5/10/20 nm 三种波长网格、高斯混合光谱曲率、sRGB/Display-P3 材质及基线批次哈希。
- `gui.py` — PySide6 预览（需求 8）：各色彩空间材质球并排色块与码值，头部标注降级阶段与链顶哈希，底部为轨迹日志。
- `acceptance.py` / `cli.py` — 验收与命令行入口。

## 环境

- Python 3.14（本机解释器，无容器/虚拟化）
- NumPy 2.5.5、PySide6 6.12.0、pytest 9.2.0（pip 本地安装）

## 验证结果（实际运行）

`python3 -m pytest -q` → 11 passed，覆盖：光谱校验定位（3 例）、并行度一致性、有序降级与配额拒绝、`E_BUSY`、单材质增量、白点变化全量重烘焙与码值偏差、强制终止恢复、断链/缺字段拒绝、GUI 离屏渲染。

`python3 -m spectral_bake.cli acceptance --workdir /tmp/acceptance_run` 实际输出：

- parallelism_independence: ok=true — 并行度 1/2/4/8 下 12 个材质球 x 2 目标空间的码值与内容哈希逐字节一致（codes_equal=true, hashes_equal=true）。
- ordered_degradation: ok=true — 单任务配额 70000 B 时严格按声明序列依次应用 reduce_sphere_samples、shrink_internal_batch、reduce_preview_resolution（84512→76320→72224→23072 B）；配额 1000 B 时抛 `E_BUDGET_EXCEEDED` 且上一发布批次完整保留（previous_batch_retained=true）。
- kill_recovery: ok=true — 子进程在提交 3 个球后被 SIGKILL（returncode=-9），journal 留存 3 条；恢复运行仅补烘焙未提交球，最终清单与未中断参考运行逐条一致（recovered_equals_reference=true）；篡改码值破坏哈希链后被 `E_RECOVERY_INVALID` 拒绝（broken_chain_detected=true）。

GUI 离屏截图：`artifacts/gui_preview.png`（并排 Display-P3/sRGB 材质球、码值、降级标注与轨迹日志）。

## 复现

```bash
pip install -r requirements.txt
python3 -m spectral_bake.cli generate-samples --out samples
python3 -m spectral_bake.cli baseline --samples samples
python3 -m spectral_bake.cli acceptance --workdir /tmp/acceptance_run
python3 -m pytest -q
python3 -m spectral_bake.cli gui --manifest samples/snapshots/batch-baseline.json --trace samples/snapshots/trace.jsonl
```
