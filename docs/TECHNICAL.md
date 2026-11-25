# 技术说明

## 1. 分层

```text
domain/       工程、约束、真值、批次、冲突、取舍、哈希
schema/       JSON 加载与 E_SCHEMA 结构校验
generator/    固定种子样本与独立参考构造真值
assembly/     纯函数装配合法性与冲突诊断
planner/      确定性首批适配合批与增量前缀重放
scheduler/    受限工作池、部件队列预算、异常隔离
dependency/   部件、挂点、材质、预算、批次、子网格依赖图
store/        SQLite 原子批次检查点与恢复
engine/       全量、增量、恢复、编辑、三路径比对
httpui/       内嵌诊断页面与本地 JSON API
cmd/wardrobe/ generate / verify / serve 入口
```

生产内核与真值构造在包级别隔离：`generator/truth.go` 不引用 `assembly`、`planner`、`engine`。

## 2. 结构校验

`schema.Validate` 汇总所有问题，任一问题格式为：

```json
{
  "code": "E_SCHEMA",
  "path": "$.projects[0].parts[3].mesh.submeshes[1].vertexCount",
  "objectId": "part:head_helm#submesh:head_helm_sm2",
  "message": "vertex interval exceeds mesh.vertexCount"
}
```

校验约束：

- bundle/project schema 版本和算法版本非空
- 工程、挂点、材质槽、部件、子网格、互斥规则 ID 唯一
- 挂点父引用、部件挂点引用、互斥部件引用、材质槽引用存在
- 子网格顶点与索引区间非负、在网格容量内、各自互不重叠
- 合批预算为正
- 真值引用存在，批次序号和提交序列可解释，真值偏移自洽

## 3. 装配判定

输入先由 `domain.SortedUnique` 排序。冲突键依次为类别、挂点、规则、部件 ID 列表和消息，确保序列化稳定。

挂点争用输出所有冲突对；显式互斥按规则 ID 输出；蒙皮不相容输出单个部件与挂点。合法子集选择顺序与合批顺序一致（优先级降序、ID 升序），每个候选只在未引入冲突时接受，因此输出是确定性的极大合法子集。

## 4. 合批与取舍

首批适配伪代码：

```text
for part in sorted(accepted, priority desc, id asc):
    for batch in batches by ordinal:
        if skinCompatible && slotsFit && verticesFit && indicesFit:
            append(part, batch)
            record reuse decision
            break
    else:
        newBatch := openBatch()
        append(part, newBatch)
        record split decision
```

取舍解释优先选择“蒙皮兼容且材质亲和”的候选批次；若不存在，选择首个蒙皮兼容批次；否则选首批阻塞批次。`blockedBy` 保留所有检查过的阻塞项，不只保留主因。

阻塞检查顺序：

1. 跨蒙皮
2. 材质槽去重预算
3. 顶点预算
4. 索引预算

## 5. 增量前沿

编辑请求克隆工程后应用，未通过结构与预算检查不会提交到引擎状态。编辑几何时重算子网格连续区间；编辑材质槽时更新部件全部子网格并将新槽版本加一。

前沿计算：

```text
direct = 旧结果中包含被编辑部件的批次序号
frontier = direct
if 编辑后部件能装入 direct 之前的某批:
    frontier = 最早可装入批次
if 新材质槽版本被 direct 之前的批次使用:
    frontier = min(frontier, 最早使用该槽的批次)
```

取 `frontier` 之前的批次和对应取舍记录作为不可变前缀，调用 `planner.PlanWithPrefix` 重放后缀。提交前比较旧前缀与重放前缀的批次内容、缓存键、依赖和决策；任何差异都触发全量回退，避免增量路径静默分叉。

### 缓存键

```text
batchCacheKey = SHA256(JSON{
  sorted(partContentHashes),
  sorted(materialSlot@version),
  constraint.version,
  constraintParamsHash,
  algorithmVersion,
  upstreamBatchCacheKey
})
```

`upstreamBatchCacheKey` 把提交序列变成显式依赖链。实际编辑时前沿算法会尽量前移，但不能证明不受影响的后缀仍会失效。

## 6. SQLite 表

```sql
checkpoint_sets(
  project_id, case_id,
  input_hash, constraint_version, algorithm_version,
  expected_batches, status, result_json,
  PRIMARY KEY(project_id, case_id)
)

batch_snapshots(
  project_id, case_id, batch_id, commit_sequence,
  part_ids_json, placements_json, budget_json, dependencies_json,
  content_hash, constraint_version, algorithm_version, cache_key,
  PRIMARY KEY(project_id, case_id, batch_id),
  UNIQUE(project_id, case_id, commit_sequence)
)
```

`Store` 使用 `MaxOpenConns(1)` 和进程内写锁。保存流程：

1. 一个事务删除旧组并写入 `building` 头
2. 每个批次一个事务插入批次行
3. 最后一批事务把组状态改为 `ready` 并写入完整结果

这使“committed batch rows”与“ready group”在同一恢复边界内。读取时逐行校验版本、内容哈希、提交序号及完整结果一致性；失败时一个事务删除整组。

## 7. 并行限制

调度器以部件权重而非任务数限制待处理队列。生产者获得权重额度后才入队；worker 接收任务时释放额度。单任务权重可以超过预算（避免死锁），但统计仍记录峰值。每个 worker 的本地候选缓冲按部件权重限制。

任务执行有 `recover` 包装；panic 转化为该任务错误。结果按原任务索引回填，因此调度完成顺序不影响批次顺序。

## 8. 三路径一致性

- full：从空缓存执行装配、合批、并行校验
- incremental：先建立旧缓存，编辑后只重放失效后缀并复用前缀
- recovery：全量结果写入 SQLite，关闭连接并重开，再按恢复协议读取

三条路径用同一结构比较器逐项检查：

- 选择与接受部件
- 合法性、冲突列表
- 批次 ID、提交序号、部件顺序、材质槽、预算
- 部件与子网格顶点/索引偏移
- 取舍记录、提交序列、依赖与缓存键

返回 `diagnostics.Mismatch`，其中包含首个差异路径、字段、批次 ID、期望值和实际值。

## 9. 异常与边界

- 结构错误返回 `E_SCHEMA`，HTTP 状态 422，附路径与对象 ID
- 无法装入空批次的部件返回 `E_BATCH_UNPLACEABLE`
- 增量前缀不一致触发全量回退并标记所有旧批次失效
- 不完整 SQLite 组不会作为可用缓存返回
- 强制终止后不生成退出报告属于明确边界；恢复只接受完整 `ready` 组
