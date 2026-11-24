# 验证记录（本地、离线、可重复）

## 环境

- Go：`go version go1.22.12 linux/amd64`
- SQLite：纯 Go 驱动 `modernc.org/sqlite v1.34.4`（go.mod 锁定，无 cgo）
- 样本种子：`42`，采样率 `16000Hz`，单声道 float32 小端 PCM

## 复现步骤

```bash
make generate          # 确定性样本：mat_main / mat_quiet / mat_empty
make test              # 单元 + 集成测试
make race              # -race 竞争检测
make demo              # 三路径端到端演示，落 var/demo_report.json
make serve             # 打开 http://127.0.0.1:8080/
```

## 样本矩阵与预期

| 素材 | 设计意图 | 预期结果 |
| --- | --- | --- |
| `mat_main` | 10 个正常特征音事件 | 三路径各检出 10 事件，类别正确率 1.0，无异常码 |
| `mat_quiet` | 事件振幅压到本底噪声量级 | 三条路径均 `E_QUIET`，details 带实测声能量占比 0 |
| `mat_empty` | 仅本底噪声、无事件 | 不报错，返回空事件列表，质量报告错配为 0 |

## 验收项对照

1. 加载与结构校验：`ingest` 测试覆盖 `E_SCHEMA`（坏类别/坏采样率）、
   `E_RANGE`（越界/重叠，含事件编号与实测边界）。
2. 指纹量化：`dsp` 测试证明重复提取与乱序提取逐位一致；越界参数返回
   `E_PARA` 并指出字段名，帧移>帧长同样拒绝。
3. 聚合与置信度：`match` 测试验证检出数、类别、区间不重叠、间隙容忍合并，
   以及空列表与 `E_QUIET`。
4. 质量校验：容差内通过；人为制造类别错配后返回 `E_QUAL` 并列出声明/实测类别。
5. 状态机：合法迁移通过，非法迁移 `E_STATE` 且原状态不变；进度按已提交/总数。
6. 检查点：SQLite 提交/恢复/整体作废测试；参数版本变化触发全量作废。
7. 三路径一致：`pipeline` 集成测试 + `compare` 单测，首个不一致定位到帧/事件字段。
8. 界面与并发：`server` 端到端测试覆盖冻结窗口 `E_STATE`、三路径比对、`E_QUIET`；
   `-race` 全绿；阻塞排队写入 `block_events`，检查点单连接串行。

## 强制终止语义

`TestCrashResumeUsesOnlyCommittedWindows` 在实时标注运行中取消上下文模拟进程
被杀死，随后以新运行视角 `RunResume`，仅依据 SQLite 中已提交窗口恢复并补齐，
最终与全量重算逐帧/逐事件一致。未提交窗口不做任何“完整报告”承诺。
