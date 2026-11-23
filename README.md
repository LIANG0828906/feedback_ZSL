# MeshForge

面向游戏美术资产的确定性三角网格修复与拓扑诊断工作台。项目只使用 Go 标准库，不下载第三方模块；样本、网格、检查点和验收报告全部写入本地目录。界面采用仅监听回环地址的 `net/http` Web 工作台，不需要数据库、容器或外部前端资源。

## 已实现能力

- 固定种子生成八面体细分球面样本，注入重复顶点、非流形附着片、翻转面、两个小孔和一个穿透球面的薄片。
- 清单记录闭合曲面真值、缺陷统计、已知点真值位置、噪声观测位置和孔洞边界。
- 严格检查索引范围、退化面、输入面数与清单一致性，`E_GEOM` 返回首个违规面片编号。
- 空间容差顶点焊接、重复面清理、附着非流形面清理、小型开放辅助片移除和稳定顶点压缩。
- 边界环识别、凸环扇形/凹环耳切三角化、闭合与新增非流形边校验。
- 基于边方向奇偶约束和闭合体积符号的面片定向传播，冲突返回 `E_ORIENT` 和分量编号。
- Taubin 拉普拉斯去噪，已知点平均误差下降，V/F/E/χ 在去噪前后严格不变。
- 自动在精细模式和空间分块模式间选择，记录候选可行性、判定依据、分块网格、每 worker 面缓冲上限和实际模式。
- 精细/分块结果对顶点、面、欧拉特征和孔洞填补面积做质量一致性校验，超差返回 `E_SCALE`。
- 六阶段状态机：`diagnose → weld → fill → orient → denoise → stats`，状态仅允许 `pending → processing → committed|failed`，失败阶段可重试。
- 每阶段原子、串行写入中间网格、内容哈希、参数版本和算法版本；重启时校验输入哈希、版本、父子哈希链和每阶段内容哈希，任一不符就整体作废并全量重算。
- 回环可交互界面：原/修复网格对照、旋转缩放、缺陷高亮、Z 剖面、阶段状态矩阵、逐阶段查看、失败重试、拓扑曲线和规模取舍记录。
- 工作线程池限制最大并行度和每 worker 网格缓冲；并行与串行运行的阶段网格哈希、最终拓扑和提交顺序相同。

详细设计见 `docs/TECHNICAL.md`，本次实测结果见 `docs/VERIFICATION.md`。

## 环境

- Windows 10/11 或 Linux
- Go 1.22+
- 仅 Go 标准库
- 不使用 Docker、数据库服务或网络依赖

确认环境：

```powershell
go version
go env
```

## 快速开始

在 Windows PowerShell 中：

```powershell
go build -trimpath -o bin\meshforge.exe .\cmd\meshforge

.\bin\meshforge.exe generate --out data --seed 20261124 --levels 5,6,7
.\bin\meshforge.exe run --sample data\small --out runs\small
.\bin\meshforge.exe run --sample data\large --out runs\large

.\bin\meshforge.exe serve --sample data\small --run runs\small --addr 127.0.0.1:8787
```

可选生成 524,288 面的上限样本：

```powershell
.\bin\meshforge.exe generate --out data-xl --levels 8
.\bin\meshforge.exe run --sample data-xl\xlarge --out runs\xlarge `
  --time-budget-ms 60000 --memory-budget-mb 2048
```

浏览器打开 `http://127.0.0.1:8787`。服务端只接受回环地址；`0.0.0.0`、局域网地址和空 host 会被拒绝。

Linux/macOS 命令相同，仅将输出路径写为 `bin/meshforge`。

## 样本

默认 `generate` 以固定种子生成三档规模：

| 样本 | 细分级别 | 输入顶点 | 输入面 | 真值顶点 | 真值面 | 真值 χ | 主要缺陷 |
|---|---:|---:|---:|---:|---:|---:|---|
| small | 5 | 4,125 | 8,193 | 4,098 | 8,192 | 2 | 1 非流形边、22 重复顶点、5 翻转面、薄片 |
| medium | 6 | 16,439 | 32,769 | 16,386 | 32,768 | 2 | 1 非流形边、48 重复顶点、5 翻转面、薄片 |
| large | 7 | 65,591 | 131,073 | 65,538 | 131,072 | 2 | 1 非流形边、48 重复顶点、5 翻转面、薄片 |
| xlarge（可选） | 8 | 262,199 | 524,289 | 262,146 | 524,288 | 2 | 1 非流形边、48 重复顶点、5 翻转面、薄片 |

每个样本目录包含：

```text
data/<id>/mesh.json   缺陷网格
data/<id>/truth.json  真值清单
```

`truth.json` 包含原始/修复后 V/F/E/χ、已知点、孔洞边界环、翻转面/孔洞面/薄片面编号，以及诊断期望值。生成过程不访问网络。

## 命令

```text
meshforge generate  生成样本与真值
meshforge run       执行六阶段修复、写最终网格与检查点
meshforge verify    校验真值、精细/分块一致性和并行确定性
meshforge serve     启动回环 Web 诊断界面
meshforge inspect   输出已持久化流水线状态 JSON
```

常用参数：

```text
--mode auto|exact|spatial
--weld-tol 1e-9
--smooth-weight 0.52
--smooth-iterations 8
--fine-face-limit 50000
--time-budget-ms 10000
--memory-budget-mb 384
--max-parallel 4
--per-worker-buffer-mb 32
--resume=false
```

验收：

```powershell
go test ./...
.\bin\meshforge.exe verify --data data --out artifacts\verification-all.json
```

## 错误码

| 错误码 | 触发条件 | 附加数据 |
|---|---|---|
| `E_GEOM` | 索引越界、退化面、面数/清单不符、诊断真值不符、最终拓扑不符 | 首个违规面片编号 |
| `E_TOLO` | 焊接容差误合并、退化边界环、填补未闭合或引入非流形边 | 建议容差 |
| `E_PARA` | 拉普拉斯权重/迭代越界、模式或资源参数非法 | 合法范围说明 |
| `E_ORIENT` | 朝向奇偶约束无法统一或统一后仍有冲突 | 冲突分量编号 |
| `E_SCALE` | 精细/分块结果的拓扑或填补面积超差，或无可行模式 | 超差项名称、两侧值、容差 |
| `E_STATE` | 非法阶段迁移、重试非失败阶段、重跑冻结阶段 | 保持原状态 |

## 规模策略

`auto` 规则是固定且可查询的：

1. 精细候选需要同时满足面数上限、连通分量上限、估算时间和估算内存。
2. 分块候选需要满足估算时间和估算内存。
3. 两个候选都可行时优先精细模式。
4. 精细不可行而分块可行时选择分块模式。
5. 两个候选都不可行返回 `E_SCALE`。

分块尺寸由面数、目标每块面数、包围盒对角线和每 worker 缓冲上限共同决定。判定记录写入 `run-report.json` 的 `state.decision`。

空间模式使用分块 broad phase 和跨块精确 narrow phase；拓扑判定仍使用精确谓词，因此两模式产生相同拓扑统计，并保留可比较的耗时/内存报告。

## 检查点

每个已提交阶段写入：

```text
runs/<id>/checkpoints/manifest.json
runs/<id>/checkpoints/stage-00-diagnose.json
...
runs/<id>/checkpoints/stage-05-stats.json
```

快照包含阶段中间网格、网格内容 SHA-256、输入哈希、参数版本、算法版本、父网格哈希和阶段矩阵。写入使用同目录临时文件、`fsync`、原子 `rename`，并由互斥锁串行化。

重启时只有以下内容全部一致才会恢复：

- 输入网格内容哈希；
- 参数版本与算法版本；
- 连续阶段序列；
- 每阶段父/子内容哈希链；
- JSON 快照中的网格内容哈希。

任一不匹配都会清除整条快照链并从头计算，不会混用旧阶段。

## 界面

回环 Web UI 提供：

- 原网格与当前/修复网格并排显示；
- 鼠标拖动旋转、滚轮缩放、复位视角；
- 翻转面、孔洞、薄片、非流形面高亮；
- 模型 Z 轴剖面裁剪；
- 阶段下拉逐阶段查看；
- 六阶段状态矩阵和失败阶段重试；
- V/F/E/χ、缺陷统计；
- 阶段拓扑曲线；
- 模式、分块尺寸、候选可行性和预算记录。

浏览器是可选查看工具，修复、诊断、校验均可在命令行独立完成。

## 实测验收摘要

本次环境：`go1.22.12 linux/amd64`。完整记录见 `docs/VERIFICATION.md`。

| 样本 | 修复后 V/F/χ | 精细耗时/峰值堆 | 分块耗时/峰值堆 | 并行=串行 |
|---|---|---|---|---|
| small | 4,098 / 8,192 / 2 | 281.9 ms / 13 MiB | 294.3 ms / 16 MiB | 是 |
| medium | 16,386 / 32,768 / 2 | 1,127.4 ms / 47 MiB | 1,093.6 ms / 62 MiB | 是 |
| large | 65,538 / 131,072 / 2 | 5,576.5 ms / 157 MiB | 5,333.1 ms / 200 MiB | 是 |
| xlarge（可选，60s/2GiB） | 262,146 / 524,288 / 2 | 27,257.9 ms / 721 MiB | 26,999.3 ms / 935 MiB | 是 |

三档样本的孔洞填补面积两侧差值为 0，`quality_fingerprint` 在重复校验中相同。耗时和堆采样属于运行观测值；拓扑/面积判定和指纹是确定性结果。

## 工程结构

```text
cmd/meshforge/           CLI
internal/mesh/           网格、邻接、边界、朝向、BVH 相交、原子文件
internal/generator/      确定性样本与真值
internal/repair/         六阶段算法、状态机、检查点、规模策略、一致性验证
internal/web/            回环 HTTP API 与内联 Canvas UI
internal/errs/           稳定错误码与结构化错误
docs/                    技术说明与验收记录
artifacts/               本次实测 JSON
```

生成数据 `data/`、运行目录 `runs/` 和二进制 `bin/` 为本地产物，默认不提交。

