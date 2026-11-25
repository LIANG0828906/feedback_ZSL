# MocapBench — 运动捕捉重定向姿态约束工作台

纯 Go 1.22+ 的确定性运动捕捉重定向、约束求解、足部接触校正、误差传播诊断与可视化工作台。SQLite 驱动使用纯 Go 的 `modernc.org/sqlite`，无 CGO、Docker、外部服务或网络依赖；样本由固定种子本地生成。

## 快速开始

```bash
go test ./... -count=1
go run ./cmd/mocapbench generate -out samples
go run ./cmd/mocapbench validate -samples samples
go run ./cmd/mocapbench verify -samples samples -clip walk_cycle -target target_hero_tall
go run ./cmd/mocapbench serve -addr 127.0.0.1:8080 -samples samples -checkpoints checkpoints
```

浏览器打开 `http://127.0.0.1:8080`。

Windows PowerShell：

```powershell
go build -o bin\mocapbench.exe .\cmd\mocapbench
.\bin\mocapbench.exe generate -out samples
.\bin\mocapbench.exe verify -samples samples
.\bin\mocapbench.exe serve -addr 127.0.0.1:8080
```

已验证 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build` 可产出 Windows amd64 EXE。

一键验收：

```bash
bash scripts/acceptance.sh
# Windows: powershell -ExecutionPolicy Bypass -File scripts\acceptance.ps1
```

实际运行记录见 `docs/validation-results.txt`。

## 交付能力

- 确定性样本：3 段动捕、2 个不同比例/关节数目标骨架、逐帧四元数/根平移/接触标记，以及关键帧世界坐标、骨长、接触区间和根轨迹真值。
- Schema 校验：缺字段 `E_SCHEMA`、父子环 `E_CYCLE`、四元数数量错误 `E_QUAT`（含帧号/关节名）、不可行映射 `E_MAP`、求解失败 `E_SOLVE`、接触越界 `E_CONTACT`、非法状态迁移 `E_STATE`、三路径差异 `E_DIFF`。
- 骨架映射：精确/别名、脊柱一对多扇出、缺失关节显式 identity 降级、未映射源关节清单、反向 Target→Source 查询、映射哈希。
- 数值求解：标准 T/R 层级 FK、关节角锥/轴向限位、末端可达 CCD 投影、确定性迭代、逐帧迭代次数与残余违背。
- 接触与根轨迹：接触区间锚点投影到 `ground_y`，根平移反向校正、指数时间平滑和最终接触投影，输出漂移与区间偏差。
- 误差传播：逐帧逐关节记录量化残差、比例残差、FK 实测传播、角度/可达残余，并给出沿深度累积的量化、比例、FK、投影和总误差上界。
- 持久化：SQLite 表 `run_meta`/`checkpoints`，每帧 JSON+哈希+参数版本+算法版本；事务提交、`sync.Mutex` 与 `SetMaxOpenConns(1)` 保证串行。版本或骨架身份不匹配时整表作废。
- 三路径：全量顺序、有界 worker pool 实时并行、强制中断后 SQLite 前缀恢复，使用同一套逐帧纯函数；自动比较旋转分量、根坐标、接触集合、统计和误差上界。
- 工作台：骨架 SVG、帧播放、层级/映射/比例表、可编辑映射与比例、误差曲线、约束容差倍数图、接触时间轴、失效传播树、路径哈希/比对和诊断日志。

## 主要目录

- `cmd/mocapbench`：`generate`、`validate`、`verify`、`serve` CLI。
- `internal/core`：样本、校验、映射、FK、求解器、接触、状态机、有界池、比对和误差模型。
- `internal/store`：纯 Go SQLite 版本化检查点。
- `internal/service`：三路径与断点恢复编排。
- `internal/web`：HTTP API 和嵌入式零依赖前端。
- `samples/`：固定种子生成的 JSON 样本、真值和清单哈希。
- `reports/`：各 clip/target 的完整 JSON 验证报告。
- `docs/TECHNICAL_NOTES.md`：算法、错误上界公式和验收说明。

## 最近一次验收摘要

- `go test ./... -count=1`：`core`、`store`、`service` 全部通过。
- 6 个 clip/target 组合全部 `VERIFY OK`。
- Hero：48/60/36 帧，最大迭代 9，最大约束残余 `9.66e-05`（容差 `1e-4`），最大接触漂移约 `2.5e-16`。
- Compact：最大迭代 6，最大约束残余 `9.48e-05`，接触漂移为 0。
- 三路径 `realtime_ok=true resume_ok=true`，恢复前缀分别为帧数一半。

完整命令输出见 `docs/validation-results.txt`。
