# Mocap Retarget Constraint Workbench

面向游戏角色动画的本地运动捕捉重定向工作台。项目从确定性样本生成、骨架校验、比例归一、逐帧约束求解、足部接触锁定，一直覆盖到 SQLite 断点恢复、三路径逐帧一致性验收和交互式诊断面板。

实现只使用 Go 标准库与纯 Go 依赖；不依赖 Docker、GPU、外部账号或远程模型。SQLite 驱动锁定为 `modernc.org/sqlite v1.29.10`（见 `go.mod`）。

## 快速开始

环境要求：Go 1.22 或更高。

```bash
go version
go env
go test ./...

go run ./cmd/mocap generate -out artifacts/samples
go run ./cmd/mocap verify-manifest -path artifacts/samples/manifest.json

go run ./cmd/mocap verify \
  -clip artifacts/samples/clips/clip_short.json \
  -target artifacts/samples/targets/target_tall_29.json \
  -mode auto \
  -out artifacts/validation/verify-short.json

go run ./cmd/mocap serve \
  -clip artifacts/samples/clips/clip_short.json \
  -target artifacts/samples/targets/target_tall_29.json \
  -mode auto -addr 127.0.0.1:8088
```

浏览器打开 `http://127.0.0.1:8088`。

Windows PowerShell 命令相同；也可以先构建：

```powershell
go build -o bin\mocap.exe .\cmd\mocap
.\bin\mocap.exe generate -out artifacts\samples
.\bin\mocap.exe verify -clip artifacts\samples\clips\clip_short.json -target artifacts\samples\targets\target_tall_29.json -mode auto
```

## 确定性样本

默认固定种子 `4208691`。同一命令重复执行时，所有 JSON 文件逐字节一致。

```text
artifacts/samples/
  manifest.json
  clips/
    clip_short.json       72 帧
    clip_medium.json      240 帧
    clip_long.json        600 帧
  targets/
    target_compact_17.json   少关节，缺失源关节显式降级
    target_tall_29.json      多关节，包含一对多映射
    target_wide_21.json      不同肢体比例与 fallback
  truth/
    clip_*.truth.json
```

每个片段包含：

- 源骨架层级、T 姿态局部偏移、关节角度上限与末端标记。
- 每帧根平移、按骨架关节顺序排列的局部旋转四元数、左右足接触标记。
- 解析真值：关键帧关节世界坐标、骨骼长度、接触区间、根轨迹起止点、约束容差。
- 目标骨架包含映射规则、fallback 规则和 `contact_joints`。

样本生成后可用清单哈希校验：

```bash
go run ./cmd/mocap verify-manifest -path artifacts/samples/manifest.json
```

## 数据校验与异常码

`internal/loader` 同时校验结构与解析真值：

- 骨架 ID、关节名、父引用、根数量、局部偏移有限性、角度上限。
- 父子链环检测；所有关节必须可达根。
- 每帧旋转数量必须等于关节数，四元数有限且单位化。
- 帧索引连续，真值 `frame_count` 与真实帧数一致。
- 真值关键帧世界坐标必须能由源骨架 FK 复算，骨骼长度必须匹配偏移。
- 接触区间合法、按序且与逐帧标记转换一致。

| 场景 | 错误码 | 定位信息 |
|---|---|---|
| 必填字段缺失、数值非法、真值骨骼长度不匹配 | `E_SCHEMA` | 对象标识、字段 |
| 父子引用成环 | `E_CYCLE` | 骨架对象、父字段 |
| 帧数或每帧旋转数量不一致 | `E_FRAME` | 对象标识、帧号、字段 |
| 目标关节无映射 / 规则引用非法 | `E_MAP` | 无法映射关节列表 |
| 迭代次数、平滑窗口、预算等参数非法 | `E_PARA` | 字段 |
| 末端可达性不可满足或迭代不收敛 | `E_INFEAS` | 帧号、关节名 |
| 接触标记缺失、未知或与真值矛盾 | `E_MARK` | 帧号 |
| 精确/采样模式接触或根端点超差 | `E_SCALE` | 实测指标 |
| 帧状态非法迁移 | `E_STATE` | 帧号、旧/新状态 |

## 核心算法

### 骨架映射与比例归一

`BuildMapping` 输出可查询、可序列化的映射轨迹：

- 源关节到目标关节支持一对一和一对多；多关节共享源旋转通道。
- `fallback_inherit` 对缺失关节做显式降级，并记录理由。
- 未被任何规则引用的源关节写入 `unmapped_sources`，不静默丢弃。
- 每个目标骨骼记录目标/源骨骼长度比值，根平移按 T 姿态整体高度比缩放。
- 每根骨骼记录从源静止方向到目标静止方向的共轭对齐四元数，保持 T 姿态坐标系一致。

### 逐帧约束求解

求解器 `retarget-v1.0.0` 使用确定性迭代投影：

1. 映射局部旋转并执行正向运动学。
2. 投影到目标关节角度上限。
3. 校验末端到目标链长的可达性；源目标点在比例归一后仍超过目标链长时立即返回 `E_INFEAS`。
4. 对仍在容差外的姿态做确定性阻尼投影，直到满足角度/可达容差或达到迭代上限。
5. 每帧记录迭代次数、角度违背、可达违背、接触漂移与归一化残余量。

短片段默认逐帧求解；未注入异常时实测约束违背为机器精度量级。

### 足部接触与根轨迹

- 每个接触区间以区间首帧支撑足位置为锚点，Y 轴锁到地面平面。
- 后续支撑帧通过根平移回推抵消足部世界位置漂移。
- 双支撑时按稳定排序选择主支撑足，避免 map 迭代顺序影响结果。
- 接触帧集合、区间起止与根轨迹端点可直接用于真值比较。

### 自适应规模策略

默认预算：

```text
max_exact_frames = 260
max_exact_joints = 34
max_exact_work   = 6000  (frames × joints)
max_solve_frames = 96
smooth_window     = 9
```

决策规则：

- 预览模式直接选择采样模式，并将关键帧上限降到 48。
- 帧数、关节数或工作单元任一超过精确预算时选择采样模式。
- 采样步长 `stride = ceil((frames-1)/(max_solve_frames-1))`。
- 强制保留首末帧和所有接触区间起止帧，避免接触集合变化。
- 关键帧逐帧求解；其余帧用四元数 SLERP 和根平移线性插值回填。
- 采样模式将平滑窗口从 9 缩短到 5，并记录原因。
- 首末帧不参与时间平滑，因此精确/采样模式的根轨迹起止点保持硬一致。

每次运行 `Plan` 都会记录模式、步长、关键帧、工作单元、平滑窗口和逐条判定依据。

### 时间平滑与冲突取舍

- 平滑在局部旋转上用三角权重四元数平均，结果与输入顺序固定。
- 平滑后先执行角度硬限位，再进行足部接触锁定。
- 约束优先于平滑：记录帧号、约束类型、动作、理由及修正前后指标。
- 同一输入下的取舍序列完全确定。

可配置参数越界时返回 `E_PARA`，例如 `smooth_window`、`max_iterations`、预算或接触阻尼。

## 状态机、SQLite 与断点恢复

帧状态：

```text
pending -> solving -> committed
                  \-> failed -> pending
```

`committed` 是终态。非法迁移返回 `E_STATE` 且保持原状态。

SQLite 使用单连接、互斥锁和每帧事务：

```text
snapshot_meta(id=1, clip_id, target_id, parameter_version,
              algorithm_version, plan_json, committed_count)
frames(frame PRIMARY KEY, status, rotations_json, root_json,
       contacts_json, stats_json, decisions_json,
       content_hash, parameter_version, algorithm_version)
```

- 每帧只允许按 `committed_count` 顺序提交。
- 内容哈希覆盖帧旋转、根平移、接触、统计、决策、参数版本和算法版本。
- 重启时先整体验证元数据和所有帧。参数版本、算法版本、内容哈希、帧序任一不匹配，就在一个事务中删除整库并全量重算。
- 不会混用旧前缀与新帧。

正常停跑示例：

```bash
# 第一次运行：提交前 17 帧后正常退出
go run ./cmd/mocap run \
  -clip artifacts/samples/clips/clip_short.json \
  -target artifacts/samples/targets/target_tall_29.json \
  -db artifacts/run/checkpoint.db \
  -stop-after 17

# 第二次运行：加载已提交前缀并继续
go run ./cmd/mocap run \
  -clip artifacts/samples/clips/clip_short.json \
  -target artifacts/samples/targets/target_tall_29.json \
  -db artifacts/run/checkpoint.db \
  -out artifacts/validation/resumed-animation.json
```

第二次输出中的 `snapshot_action` 应为 `resumed`，且 `resumed_frames` 为 17。强制终止只用于确认恢复依赖已持久化帧；仓库验证流程使用正常退出后再重启。

## 三路径一致性

`mocap verify` 自动运行：

1. **实时处理**：直接调用确定性求解内核。
2. **断点恢复**：第一次正常停止并写入 SQLite，第二次重新打开数据库续跑。
3. **全量重算**：忽略检查点重新计算。

逐帧比较关节旋转、根平移、接触集合、约束统计和迭代次数。任一字段不同即输出：

- 首个不一致帧号；
- 关节名；
- 字段名；
- 两侧数值或接触文本；
- 每帧差异热力图。

```bash
go run ./cmd/mocap verify \
  -clip artifacts/samples/clips/clip_long.json \
  -target artifacts/samples/targets/target_wide_21.json \
  -mode auto \
  -db artifacts/run/verify-long.db \
  -out artifacts/validation/verify-long.json
```

当自动模式选择采样时，`verify` 还会额外运行精确模式并检查：

- 接触帧集合差异；
- 根轨迹起点差；
- 根轨迹终点差。

超差返回 `E_SCALE`，错误消息包含实测值。

## 可视化诊断面板

`mocap serve` 启动嵌入式 HTTP 面板，无前端构建步骤和外部 CDN：

- 显著显示 `ADAPTIVE / SAMPLED` 或 `EXACT / PER-FRAME`。
- 骨架层级树、3D 投影骨架、关节角度条与末端/接触状态颜色。
- 逐帧约束违背、接触集合、根平移与迭代次数。
- 接触帧时间轴，可点击/拖动逐帧查看。
- 三条路径比较状态和逐帧差异热力图。
- 平滑/接触取舍记录。
- 映射规则、比例值与未映射源关节清单。
- 导出重定向动画 JSON。
- 重试当前帧：重新执行确定性求解并报告是否一致；不改写已提交前缀。

API：

```text
GET  /api/state
GET  /api/frame/{n}
GET  /api/export
POST /api/retry        {"frame": 12}
GET  /api/health
```

## CLI 参考

```text
mocap generate         生成确定性样本
mocap verify-manifest  校验清单 SHA-256
mocap validate         校验片段、目标、接触和映射
mocap run              执行并写入/恢复 SQLite 检查点
mocap verify           运行三路径一致性验收
mocap serve            启动诊断面板
mocap version          显示算法/模式/生成器版本
```

`run` / `verify` / `serve` 共享预算参数：

```text
-mode auto|exact|sampled
-preview
-smooth-window
-max-iterations
-angle-tolerance
-reach-tolerance
-max-exact-frames
-max-exact-joints
-max-exact-work
-max-solve-frames
-max-smooth-window
```

## 已验证结果

验证环境：Linux amd64，Go `1.22.12`；另做 Windows amd64 交叉构建检查。`go test ./...` 全部通过。

| 场景 | 计划 | 三路径 | 约束/接触 |
|---|---|---|---|
| short 72 帧 / target_tall_29 | exact，72/72 帧 | 全字段一致 | max residual `2.22e-14`，max drift `4.48e-16 m` |
| medium 240 帧 / target_tall_29 | sampled，stride 3，81/240 关键帧，smooth 9→5 | 全字段一致 | max residual `2.78e-14`，max drift `3.38e-16 m` |
| long 600 帧 / target_wide_21 | sampled，stride 7，108/600 关键帧，smooth 9→5 | 全字段一致 | max residual `0`，max drift `8.90e-16 m` |

采样/精确模式对照：

```text
contact_frame_delta = 0
root_start_delta    = 0
root_end_delta      = 0
```

断点恢复实测：

- 正常停止在 17 帧，重启后 `snapshot_action=resumed`、`resumed_frames=17`。
- 人为破坏任意帧内容哈希后，整库作废并 `snapshot_action=recomputed`、`resumed_frames=0`。
- 人为修改参数版本后，同样整体作废，不会读取旧帧。
- 非法状态迁移、乱序提交均返回 `E_STATE`。

测试还覆盖：

- 生成器重复运行逐字节一致。
- `E_SCHEMA`、`E_CYCLE`、`E_FRAME`、`E_MAP`、`E_PARA`、`E_INFEAS`、`E_MARK`、`E_SCALE`、`E_STATE`。
- 一对多映射、fallback、未映射源关节清单。
- 平滑取舍序列确定性。
- 三路径首字段差异定位。
- 面板页面、状态 API、导出与逐帧重试验证。

完整报告写入 `artifacts/validation/`。

## 设计取舍

- **精度优先路径**保留每一帧求解，适合短片段和最终烘焙。
- **采样路径**减少约束求解次数和关键帧常驻内存，但保留全帧插值输出与逐帧持久化提交。
- **缩短平滑窗口**降低采样间隔导致的时域模糊，并在计划中显式记录。
- **首末帧不参与平滑**保证根轨迹端点在两种模式下严格一致。
- **接触边界强制作为关键帧**保证接触集合不因降采样变化。
- **约束优先**允许平滑被硬限位和地面锁定修正，但每次修正均可追溯。
- **帧级事务**以吞吐换取消简单性，确保崩溃后只承认完整提交前缀。

当前实现不访问外部动作库、不下载模型，也不在验证过程中访问未授权目标。
