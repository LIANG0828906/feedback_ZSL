# BR Econ Replay

本仓库实现大逃杀赛事录像的本地确定性经济重放：空投箱槽位、玩家物资、客户端预扣除挂账、撤回补偿、网络延迟观测、哈希链、原子快照、崩溃恢复与基线分叉诊断。

实现对 `artifacts/demo` 中三组参考样本已达到逐字节一致：

- `baseline.json`
- `trace.jsonl`
- `latency.json`
- `snapshots/step_*.json`
- `snapshots/latest.json`

## 快速开始

### Linux / macOS

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt

# 验证全部参考样本
.venv/bin/python -m brecon verify --workspace artifacts/demo

# 重放一个样本，校验基线并落盘轨迹/延迟/快照
.venv/bin/python -m brecon run --workspace artifacts/demo --sample p2_s12_low

# 从最近有效快照恢复
.venv/bin/python -m brecon recover --workspace artifacts/demo --sample p2_s12_low

# 启动本地界面
.venv/bin/python -m brecon gui --workspace artifacts/demo
```

### Windows

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe -m brecon verify --workspace artifacts\demo
.\run_gui.bat
```

`run_gui.bat` 与命令行入口读取同一个 `active.json` 和样本目录，不需要网络或外部服务。

## 工作目录

```text
artifacts/demo/
  active.json                    # GUI/CLI 共用工作区清单
  verification_actual.json       # 本次实际验证报告
  gui_step30_validation.png      # 第 30 步 offscreen 界面截图
  samples/p2_s12_low/
    config.json
    operations.json
    baseline.json
    trace.jsonl
    latency.json
    snapshots/latest.json
    snapshots/step_000001.json ... step_000012.json
  samples/p4_s20_medium/ ...
  samples/p8_s30_high/ ...
```

生成新工作区：

```bash
.venv/bin/python -m brecon generate \
  --workspace artifacts/generated \
  --profiles all \
  --seed 20261121
```

默认生成 2/4/8 名玩家、12/20/30 步、低/中/高乱序三组样本。种子默认与现有清单一致：

```text
p2_s12_low      20261121
p4_s20_medium   20361124
p8_s30_high     20461127
```

生成器只使用 `random.Random(seed)`，不使用系统时钟。相同种子与输入会得到逐字节相同的 `config.json`、`operations.json`、基线、轨迹和快照。

## 时间轴与结算规则

1. 操作从输入读取后按 `arrival_seq` 稳定排序；同一 `(player_id, client_seq)` 只保留到达序最先的一条。
2. 到达步按显式输入时间计算：

   ```text
   arrival_step = max(1, ceil((arrival_ts_ms - origin_ts_ms) / step_ms))
   ```

3. 每个固定步边界，所有 `arrival_step <= 当前步` 的操作进入该步结算批次。
4. 迟到判定只使用输入时间：

   ```text
   settle_ts_ms - local_ts_ms > compensation_window_ms
   ```

   超窗操作返回 `E_OP_LATE`，不进入生效操作集，并写入轨迹、快照 `runtime.rejections` 与延迟记录。
5. 撤回意图在到达时立即标记到预扣除挂账。批次内严格按 `arrival_seq` 稳定遍历；申领处理时会读取同批已到达的撤回意图，因此即使申领序号更早也会结算为 `returned/revoked_before_settlement`，而撤回随后生成补偿记录。这样既不破坏到达序定序，也不会产生物资凭空增减。
6. 申领结算为以下之一：

   - `effective`：槽位可用、物资匹配，权威槽位归属更新，玩家物资增加。
   - `returned`：槽位已被占用，或同批撤回在结算前生效，客户端预扣除退回。
   - `conflict`：凭证物资与权威槽位物资不匹配，返回 `E_CREDIT_CONFLICT`，不改变权威库存。

7. 已结算申领不可撤回，返回 `E_REVOKE_CLOSED`。
8. 同步标记由 `config.observation.sync_steps` 显式给定。同步点比较客户端可见总量与权威持有量，差异超过 `tolerance` 时记录 `E_LATENCY_DIVERGE`。

`inspect`、`run` 与验证报告还会输出：

- `ordering`：同批申领/撤回的到达序、实际应用序、撤回意图预应用结果，以及若忽略撤回意图的对照结果。
- `conservation`：权威持有量与已占用槽位的物资守恒检查。

状态字段固定为：

```text
boxes[].slots[]: slot_id,item,quantity,available,claimed_by,claimed_client_seq
inventory[]: player_id,items[].item,items[].quantity
holds[]: player_id,client_seq,voucher_id,box_id,slot_id,item,quantity,revoke_pending_seq
revocations[]: player_id,target_client_seq,revoke_client_seq,voucher_id,box_id,slot_id,item,quantity
```

快照顶层 `state` 额外带有便于界面展示/恢复的时序元数据；哈希真值使用 `runtime.state_history[-1].state` 中的规范状态。

## 哈希与链

规范序列化是保持字段插入顺序、无空格的紧凑 JSON：

```python
json.dumps(value, separators=(",", ":"), ensure_ascii=False)
```

```text
state_hash = sha256(compact_json(state))
chain_hash = sha256(prev_chain_hash + "\n" + state_hash)
```

`prev_chain_hash` 首步使用固定模型创世哈希：

```text
9a2a09380bac1313c76d715f328ebce4fcde9f6598c76fd609782e692f735950
```

哈希输入只来自字段序列化内容，不使用时间戳、文件名、文件大小或系统时间。

快照另外包含：

```text
chain_prefix_digest = sha256(compact_json(chain))
checksum            = sha256(compact_json(snapshot_without_checksum))
```

## 原子快照与恢复

每个步边界写：

```text
snapshots/step_%06d.json
snapshots/latest.json
```

写文件流程为同目录临时文件、`flush`、`fsync`、`os.replace` 原子重命名。文件包含：

- 步号、状态、缓冲水位与完整缓冲。
- 哈希链前缀与 `chain_prefix_digest`。
- 挂账、撤回、全局对账、拒绝、延迟、同步观测与轨迹前缀。

恢复时：

1. 从 `latest.json` 开始，按 `step_*.json` 倒序尝试最近有效快照。
2. 校验 checksum、模型/房间/配置摘要、链步号、前驱哈希、链哈希、前缀摘要。
3. 校验规范状态哈希等于链头 `content_hash`。
4. 用快照缓冲继续推进，不重新应用已结算操作。
5. 字段缺失或断链返回 `E_RECOVERY_INVALID`，并携带首个断链步号和字段路径。

恢复验证的实际结果为三组样本轨迹、状态、链与基线全部一致。

## 基线比对

`brecon verify` 会逐条比较：

- `state_hash`
- `chain_hash`
- 状态字段内容
- `effective_ops`
- `reconciliations`
- 缓冲水位

第一处不一致返回：

```json
{
  "ok": false,
  "code": "E_STATE_DIVERGENCE",
  "step": 5,
  "field_path": "inventory[0].items[0].quantity",
  "expected": 999,
  "actual": 0,
  "effective_ops": [],
  "buffer_depth": 19,
  "buffer_before": 20,
  "buffer_after": 19
}
```

错误码：

| 错误码 | 含义 |
| --- | --- |
| `E_OP_LATE` | 操作超过补偿窗口 |
| `E_CREDIT_CONFLICT` | 预扣除凭证与权威库存冲突 |
| `E_REVOKE_CLOSED` | 目标已结算，撤回关闭 |
| `E_LATENCY_DIVERGE` | 同步窗口客户端/权威差异超容差 |
| `E_RECOVERY_INVALID` | 快照缺失、checksum/链/字段校验失败 |
| `E_BASELINE_VERSION` | 基线版本、模型版本或配置摘要不匹配 |
| `E_BASELINE_INVALID` | 基线缺失、截断或步序列不连续 |
| `E_STATE_DIVERGENCE` | 首个状态/事件序列分叉 |
| `E_BUSY` | 同房间已有推进任务 |

同一房间互斥使用进程级文件锁：POSIX `flock`，Windows `msvcrt.locking`。进程被强制终止时锁由操作系统释放，不会形成永久 stale lock；重复启动返回 `E_BUSY`。

本环境并发实测：持锁进程运行时，第二个 `brecon run --sample p2_s12_low` 返回退出码 `1`，错误为 `E_BUSY`。

## 图形界面

```bash
.venv/bin/python -m brecon gui --workspace artifacts/demo
```

界面包含：

- 每步进度与当前步生效/拒绝数量。
- 空投箱槽位余量与归属。
- 玩家权威物资与挂账可见量。
- 每步玩家总物资曲线。
- 挂账与撤回列表，可选中挂账注入撤回。
- 延迟直方图、P50/P95/P99、补偿命中率与同步窗口表。
- 轨迹日志。
- 启动、暂停、撤回注入、从最近有效快照恢复。

无桌面环境可做冒烟截图：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m brecon gui \
  --workspace artifacts/demo --sample p8_s30_high \
  --offscreen --auto-run \
  --screenshot artifacts/demo/gui_step30_validation.png
```

## 测试与实测结果

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
```

本环境实测：

```text
Ran 12 tests
OK
```

覆盖内容：

- 三组参考产物逐字节一致。
- 每一步快照逐字节一致。
- 基线逐条校验。
- 三组样本中点快照恢复后轨迹/状态/链一致，无重复申领。
- 预扣除在同步标记处与权威状态收敛。
- 基线版本错误与截断错误。
- 首处分叉字段路径与上下文。
- 快照 checksum/链断裂定位。
- `E_BUSY` 互斥锁。
- 生成器确定性、迟到/冲突/撤回关闭链路。
- PySide6 offscreen 自动运行与截图。

三组参考样本最终哈希：

| 样本 | head_state_hash | head_chain_hash | 恢复点 |
| --- | --- | --- | --- |
| `p2_s12_low` | `3e3c3d126542782e7730b2ab332c7b410a0ef74dad1e3b887d04f0ac2bf6f38f` | `0f441fa371f8090e34eb472604d338559d9ee84944d29fce8565780bcc3d4f12` | step 6 |
| `p4_s20_medium` | `7f4b32092064d9da0e4e78cdf223c6830e5e260997a70e19f97a9f713c958ff2` | `d17a3bf2d3c405824e2a9be6fde5128176f8ffc435265a8f20d7577727acf534` | step 10 |
| `p8_s30_high` | `1e9b14e2b2f23397b67f6bd285b79f3062011c93dbfcb0da3bcfaa428247b7b7` | `e74de54994ebbd535a449cf250e61581a64cfab9e316273e2eabdd32b8775d8d` | step 15 |

延迟与补偿：

| 样本 | n | mean | P50 | P95 | P99 | 补偿命中 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `p2_s12_low` | 28 | 90.5ms | 75ms | 95ms | 474ms | 3/3 |
| `p4_s20_medium` | 48 | 124.979ms | 95ms | 187ms | 486ms | 5/5 |
| `p8_s30_high` | 78 | 246.205ms | 186ms | 663ms | 775ms | 8/8 |

完整实测报告见 `artifacts/demo/verification_actual.json`，逐字节轨迹见各样本的 `trace.jsonl`。

