# 设计说明

## 快照边界

每条单页查询使用 `SNAPSHOT` 事务，计划和数据在同一个事务内读取。整轮分页由
`SnapshotPagingSession` 持有一个事务和一条租约连接，因此同一轮游标遍历不会观察
到中途提交的更新。

游标使用唯一键 `OrderId` 作为最终决胜列：

- 旧键序按 `Status, OrderId`；
- 新键序按 `TenantId, Status, UpdatedAt, OrderId`。

## 锁顺序

所有业务读和索引 DDL 都遵守：

```text
IndexChangeBarrier -> ConnectionGate -> SQL Server
```

`IndexChangeBarrier` 是写者优先的异步读写屏障。读侧在 DDL 判定后开始前进入；
写侧在 `DROP/CREATE INDEX` 前进入。写者等待期间阻塞新读者，因此不会饥饿，也不会
出现 DDL 与查询交错导致的计划变化。

SQL Server 自身仍会在 DDL 期间持有 Sch-M 锁，在快照读期间持有 Sch-S 锁。屏障
负责网关内部时序，引擎锁负责与网关外连接互操作。

## 连接池

`ConnectionGate` 是连接并发数的唯一权威上限。ADO.NET 连接字符串的
`Max Pool Size` 同步设为该上限。租约释放时先关闭/归还 SQL 连接，再释放信号量。
获取槽超时直接返回 `E_CONN_TIMEOUT`，不占用连接；命令或计划捕获异常由
`await using` 保证归还。

## 索引生命周期

- `DUAL`：两索引共存，两种键序各自命中。
- 旧实例心跳活跃：拒绝删除旧索引。
- 心跳全部过期：删除旧索引，新键序继续命中新索引。
- 回退：删除新索引、重建旧索引；新键序使用无指定索引的 `FORCESEEK` 回退路径。
- 验证结束：恢复双索引，保证重复执行基线一致。

## 可观测性

指标只保存逻辑值。错误上下文限制为过滤条件、键序、索引名、访问方式、SQL 错误号
和超时阈值；报告不写入连接串、密码、主机名或绝对路径。
