# 游戏 UI 图标掩码精修内核

这是一个从空目录独立实现的 Go 领域组件与本地诊断工作台。样本、粗糙初始掩码、解析真值、边缘带、自适应半径、分块修复、预览/精确双路径和 SQLite 检查点均可在本机确定性复现。

## 快速开始

环境要求：Linux / WSL Ubuntu、Go 1.22+、可写的本地目录。运行期不需要网络、外部数据库、Docker 或其他服务。

```bash
go version                 # 本机验证：go1.22.12 linux/amd64
gofmt -w .
go test ./...
go vet ./...

# 一键验收并写入 artifacts/verification.json
go run ./cmd/maskverify

# 启动 localhost 诊断台（默认 127.0.0.1:8099）
go run ./cmd/masklab
# 打开 http://127.0.0.1:8099
```

SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。首次准备模块时可联网下载依赖；依赖进入本地模块缓存后，运行和测试不需要外部服务。

```bash
go mod download
GOPROXY=off go test ./...
```

## 交付结构

```text
.
├── cmd/masklab/          # net/http 交互式诊断台
├── cmd/maskverify/       # 一键生成声明式验收报告
├── generator.go          # 固定种子样本与解析真值生成器
├── schema.go             # 工程加载、尺寸/字段/边界段校验
├── morphology.go         # square/disk/diamond/cross 形态学运算
├── edge.go               # 形态学梯度边缘带与初始曲率估计
├── radius.go             # 曲率分段半径映射与单调性校验
├── repair.go             # 分段半径驱动的闭/开交替修复
├── blockrepair.go        # 带 halo 的真实固定块迭代
├── state.go              # 阶段/轮次双层状态机
├── dependency.go         # 缓存键、依赖图、精确失效传播
├── preview.go            # 降采样预览、全精度重算与 E_DIVERGE
├── checkpoint.go         # SQLite 单事务阶段检查点
├── scheduler.go          # 并行度、像素缓冲、队列预算
├── consistency.go        # 增量/恢复/全量三路径比对
└── artifacts/
    ├── samples/          # project.json 与 3 组 PNG 样本
    └── verification.json # 最近一次实际验收结果
```

## 领域实现

### 1. 确定性样本与真值

`GenerateProject(seed)` 生成 `icon-96`、`icon-128`、`icon-160` 三张图标。图标由椭圆色块、圆环前景（含一个解析孔洞）和近色背景渐变组成，并叠加半透明描边和固定 PRNG 噪声。初始掩码在真值上构造三类缺陷：

- 边界内缩缺口；
- 外溢凸块与孤立碎点；
- 封闭小孔。

真值组件记录面积、重心、外接框、孔洞数；参考掩码为单通道二值。每条边界环按归一化弧长划分为 8 段，记录 `[start,end)`、解析中点曲率和采样点。环从 `0` 连续覆盖到 `1`，段起点严格递增。

加载时校验：

- 图像、初始掩码、参考掩码分辨率一致；
- 掩码数据长度等于 `width*height` 且值只能是 `0/1`；
- 组件、边界段、曲率、采样点字段完整；
- 同一组件/边界环的段连续覆盖 `[0,1)` 且起点严格递增。

冲突统一返回 `E_SCHEMA`，并携带 `imageId` 与具体 `field`。

### 2. 边缘带与初始曲率

`LocateEdgeBand` 使用用户声明的结构元素形状与半径带下限：

```text
edgeBand = dilate(mask, se) - erode(mask, se)
```

支持 `square`、`disk`、`diamond`、`cross`。按真实边界段采样点寻找初始掩码最近边界像素，并做局部代数圆拟合，输出逐段初始曲率估计。空边缘带不被当作成功，而是返回 `status=empty_edge_band`、`Empty=true`、`Success=false`。

### 3. 自适应半径

半径映射只依赖真值曲率、全表曲率范围、半径带 `[min,max]` 与分段数：

```text
normalized = (curvature - minCurvature) / (maxCurvature - minCurvature)
bucket     = floor(normalized * curvatureBands)
radius     = maxRadius - round(bucket * (maxRadius-minRadius)/(curvatureBands-1))
```

因此曲率越大的段映射半径不会更大，且结果一定是声明带内正整数。`RadiusTable` 逐段输出曲率、曲率半径、归一化值、桶、腐蚀/膨胀半径。`ValidateRadiusTable` 会检查：

- 半径是否为带内正整数；
- 是否可由表中真值曲率完全复算；
- 高曲率段半径是否不超过低曲率段。

带外值或非单调映射返回 `E_RADIUS`，携带段 ID；单调性错误同时记录左右段标识、曲率和半径。

### 4. 分块闭/开交替修复

边缘带像素按最近真实边界段采样点分配，每个段独立使用自己的半径：

1. 闭运算填补内缩和孔洞；
2. 以闭运算结果为输入执行开运算，削除外溢和孤立碎块；
3. 每轮记录闭/开变化像素数、整轮变化数和残余；
4. 在预算内达到固定点则成功，否则返回 `E_ITER`，携带最后一轮与遗留像素数。

`RefineBlockwiseParallel` 将图像切成固定块，块周围读取宽度为 `maxRadius*rounds + minRadius + 2` 的 halo，每次只提交块核心；块内形态学轮次由受限工作池并行执行。验证结果显示块级结果与整帧修复逐像素一致，默认 2 个 worker 的峰值并行度为 2。所有修复只提交边缘带内像素，带外像素逐点不变。

### 5. 状态机

每个块、每个轮次有四个阶段：

```text
radius -> close -> open -> stats
```

每个阶段状态为 `pending -> processing -> committed`，处理中可转 `failed`，失败后可 `retry` 回到 `pending`。已提交阶段不可再次提交或编辑。进度分别维护：

- `committedStages`：累计已提交阶段数；
- `committedRounds`：已提交 `stats` 阶段形成的轮次数。

非法迁移返回 `E_STATE`，不改变原状态与进度。`ApplyEdit` 只允许重置未提交阶段及其下游，并拒绝修改冻结的已提交产物。

### 6. 依赖图与精确失效

缓存键为以下字段的 SHA-256：

```text
block content hash + segment id + radius + round + upstream cache key
```

依赖图显式记录：

- 边界段曲率 -> 段半径；
- 半径带/曲率分段数 -> 段半径；
- 段半径/运算轮次 -> 下游块掩码分量。

参数变化时先重算每个段的新半径，只使半径实际变化的段及其下游块失效。初始掩码编辑时按变化像素附近最近的真实段确定受影响段，再传播到依赖块。失效清单包含块坐标、段标识、原因、缓存键及旧/新半径。`ReusableCheckpoints` 只复用仍有效块的持久化阶段产物。

### 7. 预览与精确双路径

拖动参数时：

- 使用面积多数投票降采样（默认倍率 2）；
- 缩放边界段与半径带，仅保留 2 轮；
- 最近邻上采样回原分辨率；
- 记录实际倍率、轮次预算、耗时、是否在声明的延迟预算内。

停止交互后使用同一 `ParameterSnapshot` 版本执行全精度修复。比较区域数、各区域重心欧氏偏差、面积相对误差。任一项超差返回 `E_DIVERGE`，同时给出预览/精确实测值、容差、参数版本和首个差异像素坐标。

### 8. SQLite、并行与三路径

每个块每阶段提交时，在一个 SQLite 事务中写入：

`output_digest / radius_table_json / cache_key / content_hash / parameter_version / algorithm_version / output_mask`

存储层单写锁 + 单连接保证事务串行、不交错；重复阶段键以 `E_CHECKPOINT` 失败且不产生半提交。

并行调度器限制：

- `MaxWorkers`；
- 每任务像素缓冲上限；
- 待处理队列容量；
- 全局像素缓冲令牌。

队列或像素令牌耗尽时生产者阻塞，并记录 `queue_budget` 或 `pixel_buffer_budget` 原因与实际等待时长。

`RunThreePathConsistency` 分别运行：

1. 实时增量：按块/轮/阶段顺序提交检查点；
2. 断点恢复：写入前缀检查点后关闭数据库，重新打开并从已提交键继续；
3. 一次性全量重算：不读取检查点。

三条路径比较最终掩码、区域统计和阶段提交顺序；不一致时定位首个不一致块。

## 诊断台

默认只监听 `127.0.0.1:8099`，使用 Go 标准库 `net/http`，页面不加载外部资源。

```bash
go run ./cmd/masklab \
  -addr 127.0.0.1:8099 \
  -seed 42 \
  -db artifacts/masklab.db \
  -samples artifacts/samples
```

主要接口：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/health` | 版本、种子、样本数 |
| GET | `/api/samples` | 样本与真值摘要 |
| POST | `/api/run` | 参数化双路径 + 三路径诊断 |
| GET | `/api/mask?image=icon-128&kind=initial` | 初始/真值/边缘/修复/预览 PNG |
| GET | `/api/source-image?image=icon-128` | 确定性源图 PNG |
| GET | `/api/checkpoints?image=icon-128` | 最近一次阶段提交序列 |
| GET | `/api/project` | 完整工程 JSON |

参数错误返回 HTTP 400；迭代预算耗尽返回 409；预览分歧返回 422；其余异常返回对应 JSON 错误码。

## 实际验证结果

本机执行：

```text
$ go test ./...
ok  	example.com/iconmask
?   	example.com/iconmask/cmd/masklab	[no test files]
?   	example.com/iconmask/cmd/maskverify	[no test files]

$ go vet ./...
# 无输出

$ go run ./cmd/maskverify
icon-96  iou=0.9561 regions=11/3 preview=1.480/0.091 3path=true commits=108
icon-128 iou=0.9798 regions=7/3  preview=0.354/0.044 3path=true commits=128
icon-160 iou=0.9833 regions=11/3 preview=0.398/0.057 3path=true commits=200
report=artifacts/verification.json allChecksPassed=true
```

固定种子 `42`、默认参数下的关键结果：

| 样本 | 初始/真值/修复区域 | IoU | 块/整帧一致 | 真值重心误差 | 真值面积误差 | 预览重心误差 | 预览面积误差 | 三路径一致 |
|---|---:|---:|---|---:|---:|---:|---:|---|
| icon-96 | 11 / 3 / 3 | 0.9561 | 是 | 0.255 px | 0.0211 | 1.480 px | 0.0909 | 是 |
| icon-128 | 7 / 3 / 3 | 0.9798 | 是 | 0.239 px | 0.0286 | 0.354 px | 0.0439 | 是 |
| icon-160 | 11 / 3 / 3 | 0.9833 | 是 | 0.222 px | 0.0176 | 0.398 px | 0.0574 | 是 |

三个样本均：

- 边缘带外变化像素数 `0`；
- 预览实际降采样倍率 `2`、使用轮次 `2`，均在上限内；
- 预览/精确区域数一致；
- 增量、恢复、全量掩码与区域统计逐像素/逐字段一致；
- 阶段提交序列一致；
- 首个不一致块为空。

异常码实测：

| 错误码 | 触发方式 | HTTP |
|---|---|---|
| `E_SCHEMA` | 掩码宽度冲突、非二值、真值缺失、段覆盖断裂 | 400/加载失败 |
| `E_PARA` | 非法结构元素、半径带下限非正整数 | 400 |
| `E_RADIUS` | 半径带外、不可复算、单调性破坏 | 400 |
| `E_ITER` | 预算 1 轮仍有余量 | 409 |
| `E_STATE` | 阶段乱序/冻结后编辑 | 本包/HTTP 使用处 |
| `E_DIVERGE` | 面积容差设为不可满足值 | 422 |

`artifacts/verification.json` 中的 `errorCodes` 会记录本次所有异常码是否实际观察到。

## 验证覆盖

测试文件覆盖：

- 同种子逐结构相等、不同分辨率、PNG/JSON 往返；
- schema 缺字段、尺寸冲突、非二值、段间断裂；
- 形态梯度、空边缘带显式状态、`E_PARA`；
- 半径带内、可复算、单调性、`E_RADIUS`；
- 默认收敛、`E_ITER`、带外零变化、块级/整帧逐像素一致；
- 状态机合法/非法迁移、冻结、失败重试、进度保持；
- 缓存键五个输入维度、参数精确失效、掩码编辑传播、有效块复用；
- 预览/精确参数版本、延迟、容差和 `E_DIVERGE`；
- SQLite 并发串行、重复事务原子失败、持久化读取；
- 并行度上限、队列等待、像素缓冲预算；
- 三路径掩码/统计/提交序列一致及检查点元数据完整。

## 说明与边界

- 真值仅用于构造、校验、精度观测和收敛/分歧判定；修复操作本身只读取初始掩码、边界段计划和参数。
- 修复是离散形态学近似，不承诺逐像素恢复解析真值；实际默认样本 IoU 为 `0.9561`–`0.9833`，区域数和孔洞数与真值一致，最大真值重心偏差约 `0.22`–`0.25 px`，最大面积相对误差约 `1.8%`–`2.9%`。
- 预览是交互近似路径，声明容差默认 `3 px` 重心、`22%` 面积相对误差；可用环境参数收紧并观察 `E_DIVERGE`。
- `artifacts/samples` 与 `artifacts/verification.json` 是本次固定种子生成产物，可全部删除后由命令重新生成。

