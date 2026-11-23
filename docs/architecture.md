# 架构与实现说明

## 分层

```text
story     工程 JSON 模型、严格加载、语义校验、因果拓扑、显式依赖图、内容哈希
engine    确定性状态机、逐镜记录、版本化存档、续演、比较、失效重演
checkpoint SQLite 单写事务、逐镜检查点、整体校验和整体作废
worker    固定大小有界线程池
samplegen 固定种子多场景样本生成与真值回流
workbench 样本服务、保存索引、三路径并行比对、HTTP API
webui     内嵌 HTML/CSS/JS 桌面式演示界面
```

## 工程加载与校验

`story.LoadProject` 先以 `json.Decoder.UseNumber` 读取原始对象，检查必需字段存在性，再进行严格类型解码。这样能区分：

- 字段缺失：`E_SCHEMA`
- 小数/字符串等非定点整数：`E_PARA`
- 负时长、零时长、超过上限：`E_PARA`
- 引用不存在、重复 ID：`E_SCHEMA`
- `join` 图成环：`E_CYCLE`

节点顺序不参与因果正确性，但作为拓扑排序的稳定平局裁决。依赖图包含：

- `join`：因果汇聚边
- `reads`：节点事件写入字段到另一节点前置条件读取字段的数据依赖
- `mutex`：共享互斥对

## 确定性状态机

会话状态只包含语义数据：

```text
flags, characters
choice_offset
node_states(activated, completed, activation_order)
event_order
shots
clock_ms
current_node_id, current_node_next_shot
current_choice_index, current_choice_at_ms
```

单步循环：

```text
if current node has remaining shots:
    record shot at clock_ms
    clock_ms += duration
    advance current shot index
else if choices remain:
    consume next choice
    if target is ineligible: record blocked reason
    else: apply events, activate node, clock_ms = max(clock_ms, choice.at_ms)
else:
    done
```

状态迁移中没有 map 迭代顺序、goroutine ID、完成顺序或 wall clock 参与。地图仅用于查找，所有影响输出的列表都按声明顺序、数组顺序或拓扑顺序生成。

## 存档协议

`Save` 分为三层校验：

1. 结构：`schema_version`、必需字段、镜头序号、镜头进度。
2. 身份：`project_id`、工程 `content_hash`、`algorithm_version`。
3. 语义：对 `done + state` 做 SHA-256 `fingerprint`，再校验镜头定义、选择目标、呈现时间和播放时钟。

续演从保存的当前节点、当前镜头索引、事件应用后的状态和播放时钟恢复。因为事件在节点激活时已全部应用，存档只可能落在镜头边界，所以无需重放半个节点的事件。

## 失效闭包

编辑只允许替换节点 `requires` 或 `events`。失效计算对旧图与编辑后图取并集，从编辑节点 BFS：

- `join` 下游：节点语义依赖其前置节点完成。
- `reads` 下游：编辑事件会改变被其他节点条件读取的字段。
- `mutex` 双向：一个节点的可达性可能改变互斥节点的可达性。

输出包含：

- 失效节点标识及原因
- 旧运行中失效镜头序号
- 可复用旧镜头序号
- 编辑后重演按拓扑序排列的失效节点

重演使用完整确定性执行作为 oracle。每个未失效节点的新镜头只有在节点、镜内序号、时长、画幅、呈现时刻、选择、描述和状态哈希都一致时才复用旧记录。否则返回 `E_DIVERGE`。

## 三路径比较

`workbench.CompareThreePaths`：

1. 连续路径：从工程起点推进编辑后工程。
2. 存档续演：推进到保存边界，序列化并重新解析存档，再续演。
3. 编辑重演：对基线运行执行失效闭包和原子重演。

三条路径在固定线程池中并行运行。结果按作业索引收集，不按完成顺序。两两调用 `engine.CompareRuns`，逐项比较：

- 镜头数量与顺序
- 节点、镜内序号、时长、画幅、描述、呈现时刻
- 选择索引与逻辑时间
- 每个镜头的状态哈希
- 终态标志位、角色状态
- 事件到达顺序
- 可达节点
- 阻塞选择
- 总时长及逐节点时长统计
- 最终状态哈希

首个差异返回 `E_DIVERGE`，包含 `shot_sequence`、`node_id`、`field`，并在 `artifact_dir` 保留全部路径 JSON。

## SQLite 表

```sql
checkpoint_meta(
  project_id PRIMARY KEY,
  content_hash,
  save_schema_version,
  algorithm_version,
  last_shot_seq,
  updated_at
)

shot_checkpoints(
  project_id,
  shot_seq,
  node_id,
  node_shot_index,
  choice_offset,
  content_hash,
  save_schema_version,
  algorithm_version,
  shot_json,
  save_json,
  created_at,
  PRIMARY KEY(project_id, shot_seq)
)
```

`Store.CommitBoundary` 使用：

- 单个 `database/sql` 连接
- `sync.Mutex`
- 单个 SQLite 事务
- 连续序号检查
- 重复提交指纹幂等

`LoadLatest` 在读取时检查元数据、所有行、每行保存载荷、行列与载荷一致性、连续性和最新指纹。任一失败会在事务内删除该工程全部元数据与镜头行，再返回 `E_SAVE`。

## 资源边界

- `workers`：固定工作线程数。
- `buffer_per_worker`：每个线程独立队列容量。
- 提交到已满队列会阻塞。
- 每个任务结果写入容量为 1 的私有通道，不会因主收集循环阻塞而丢结果。
- 共享 SQLite 写入由 Store 互斥锁和单连接串行化。

## 可复现性

- 样本生成使用自实现 xorshift 序列，不依赖版本相关的全局随机源。
- JSON 内容哈希使用标准库稳定 map key 排序。
- 所有排序都有明确二级键。
- 保存与检查点包含算法版本，算法行为变化会使旧存档整体失效。

