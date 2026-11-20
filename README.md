# 确定性回合制对局回放系统

赛事历史对局回放与一致性验证平台：规则引擎按回合重放、并行调度受全局资源池
约束、快照支持中断恢复、基线逐回合哈希比对，并提供 PySide6 图形界面与
确定性样本生成器。相同输入、配置与调度顺序下输出**逐字节一致**。

## 环境要求

- Python 3.11+（开发验证环境为 Python 3.14 + numpy 2.5 + PySide6 6.12）
- 不使用 Docker 或任何容器；全部为本地文件与本地线程

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
source .venv/bin/activate
pip install -r requirements.txt
```

## 快速开始

```bash
python -m replay.cli init-config           # 生成 config/replay.json
python -m replay.cli generate              # 生成样本与基线（含预算过紧样本）
python -m replay.cli run                   # 并行回放 + 基线验证
python scripts/acceptance.py               # 一键完整验收
python -m replay                           # 启动图形界面
```

Windows 可直接执行 `scripts\run_windows.bat` 完成上述全部步骤。

## 常用命令

```bash
# 小预算配置，演示 E_BUDGET_EXCEEDED
python -m replay.cli --config config/replay.tight.json run \
    --output output/tight --match tight_budget_s05

# 从已提交快照恢复（快照链校验失败返回 E_RECOVERY_INVALID）
python -m replay.cli run --recover

# 单任务基线比对
python -m replay.cli verify small_s01 samples/baselines/small_s01.baseline.json

# 制造基线差异并查看差异定位（首个不一致回合 + 字段路径）
python -m replay.cli tamper-demo small_s01 samples/baselines/small_s01.baseline.json
```

## 目录结构

```
config/            资源池并发上限与单任务预算（冻结配置，运行期不可改）
replay/            回放器源码（引擎、调度、快照、验证、GUI、样本生成）
tests/             16 个单元/集成测试
scripts/           一键验收脚本与 Windows 批处理
samples/matches/   对局描述样本与 manifest
samples/baselines/ 权威基线（真值来源）
output/            快照、结果、轨迹日志与验证报告（git 忽略）
docs/TECHNICAL.md  架构与确定性设计说明
```

## 关键设计

- **确定性**：仅使用种子化 `numpy PCG64`；RNG 完整状态随快照保存恢复；
  JSON 规范化排序、UTF-8/LF、无时间戳；资源计量由输入公式推导。
- **原子快照**：`*.tmp` + `fsync` + `os.replace`，在途回合不落盘；
  SHA-256 哈希链逐节点校验。
- **预算强制**：累计超预算即终止并返回 `E_BUDGET_EXCEEDED`，
  不静默降级；恢复后沿用快照中的累计用量继续计量。
- **并行调度**：任务间并行、任务内回合严格顺序；有界信号量限制并发，
  等待任务按提交序号稳定排序。
- **协作取消**：只在回合边界与资源检查点响应，释放槽位并保留快照。
- **验证**：版本闸门先行（`E_BASELINE_MISMATCH`），再比对判定、
  事件流哈希与逐回合快照哈希，报告首个差异位置。

## 测试与验收

```bash
python -m unittest discover -s tests -v
python scripts/acceptance.py
```

最近一次实际验证结果：16 个测试全部通过；验收脚本确认两次独立运行
输出目录逐字节一致、预算超限正确报错、快照恢复与全量运行逐字节一致、
篡改基线可定位到具体回合与字段路径。详见 `docs/TECHNICAL.md`。
