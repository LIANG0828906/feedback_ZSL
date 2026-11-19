# 一致性设计

## 角色与事实来源

- **etcd**：租约、当前锁、规范成果和释放事件的唯一持久化事实来源。
- **coordinator**：执行 etcd 原子事务、维护进程内 FIFO 队列、提供 gRPC/HTTP 视图。
- **worker A/B/C**：独立进程，通过 gRPC 领取批次；只把协调服务接受的规范成果写入 SQLite。

锁 key 布局（`key_prefix` 默认 `/settlement/v1`）：

```text
/settlement/v1/locks/<batch>           带 etcd lease 的锁记录
/settlement/v1/current/<batch>         最近一次锁 generation（用于见证过期）
/settlement/v1/results/<batch>         唯一规范成果
/settlement/v1/idempotency/<batch>/<holder>  首次提交持有者别名
/settlement/v1/release-events/<batch>/<generation>  不可变释放事件
```

锁记录包含 `batch_id`、`holder_id`、`generation`、`lease_id`、`lease_ttl_ms`、
`expires_at_unix_ms` 和 `acquired_at_unix_ms`。

## 互斥申请

1. 参数校验；租约小于 3000 ms 时立即返回 `E_LEASE_TOO_SHORT`，不做任何写操作。
2. 读取锁 key 和 `ModRevision`。
3. 若仍由同一 holder 持有且未过期，直接执行续约并返回 `renewed=true`。
4. 若由其他 holder 有效持有，返回 `E_LOCK_HELD`、当前持有者和当前到期时间。
5. 若锁已过期，使用 `Compare(ModRevision == 读取值)` 删除；只有事务成功者继续。
6. 创建 etcd lease，再以 `CreateRevision(lockKey) == 0` 原子创建锁和 `current` 见证。
7. 若并发创建失败，立即 revoke 新 lease，并重新读取当前持有者。

revision 比较消除了“读取到过期锁后，原持有者恰好续约成功”的竞态：续约会改变
`ModRevision`，旧清理事务因此失败，不会误删新租约。

## 续约

- 同 TTL：`KeepAliveOnce` 刷新 etcd lease，随后以 `ModRevision` 条件原子写回新到期时间。
- 改变 TTL：先申请新 lease，以条件事务把锁切换到新 lease，再 revoke 旧 lease。
- etcd 返回 lease 不存在、锁被替换或到期时间已过：返回 `E_LOCK_LOST`。
- 租约过短：在 KeepAlive/事务之前拒绝，因此原到期时间与 holder 均不变。

## FIFO 等待

协调服务为每个批次维护按到达顺序分配的 ticket 队列：

1. 每个申请先入队，即使 `wait_timeout_ms=0`，避免后来的零等待请求插队。
2. 只有队首允许尝试 etcd 获取；其余请求等待通知或 100 ms 逻辑轮询。
3. 释放、过期、队首超时或离开都会唤醒队列。
4. 队首在 deadline 前可反复尝试；超过 deadline 返回 `E_WAIT_TIMEOUT` 并移出队列。
5. 队首移出后，队列显式唤醒下一位，避免信号丢失。
6. 当前持有者的重复申请在入队前按续约处理，不会排在其他等待者后面。

协调服务重启会丢失内存队列，但 etcd 锁和租约仍正确；客户端看到 RPC 错误后重试即可重新排队。

## 过期与释放事件

三重机制保证过期状态和释放事件收敛：

1. 申请/状态查询发现 `expires_at_unix_ms <= now` 时，条件删除锁。
2. 协调服务每 150 ms 扫描锁；对已过期锁执行条件删除。
3. etcd watch 监听删除事件；若没有显式释放事件，则补写 `lease_expired`。

显式 `ReleaseLock` 在同一事务中写 `released` 事件并删除锁。事件 key 由 generation 唯一化，
所有补写使用 `CreateRevision == 0`，因此至少一次触发不会产生重复事件。

`current` 见证了最近 generation，可让协调服务在 watch 短暂断连或重启后补齐缺失事件。

## 成果提交

提交前检查：

1. 锁存在。
2. 请求 holder 与锁 holder 一致。
3. 到期时间尚未过去。

然后以 `ModRevision(lock) == 读取值` 且规范结果 key 不存在为条件，在一个 etcd 事务中写：

- `/results/<batch>` 规范成果；
- `/idempotency/<batch>/<holder>` 首次提交持有者别名。

只有事务成功的任意一个并发提交会成为规范成果。后续提交：

- 同持有者：返回规范成果，`idempotent=true`；
- 租约释放后的合法新持有者：同样返回首次规范成果，`committed_by` 不变；
- 已经失锁的旧持有者：锁检查先失败，返回 `E_LOCK_LOST`，即使规范成果已经存在也不写任何东西。

## SQLite 收敛

- 规范成果落地到 `settlements` 与 `settlement_tasks` 的同一个 SQLite 事务。
- 重复落地先校验摘要与 payload；一致时幂等返回，不一致时报错且不覆盖。
- busy timeout 之外，写入函数对 `database is locked` / `SQLITE_BUSY` 重试最多 30 秒。
- 状态接口返回规范 `result_json`。worker 每轮检查已提交批次是否存在于共享 SQLite；
  缺失时用规范成果补齐，因此“协调已提交、worker 在写库前崩溃”可由任意存活 worker 收敛。
- 未获得协调服务接受的批次不会写入任何 SQLite 行。

## worker 续约失败

每个批次处理期间有独立续约循环。续约 RPC 失败或响应 `renewed=false` 时：

1. 向处理循环发送 lost 信号。
2. 处理循环停止处理后续任务，丢弃尚未提交的内存结果。
3. 提交前再次比较本地到期时间，覆盖 GC 长暂停期间信号尚未送达的情况。
4. 若提交已被协调服务接受，则该成果已经是规范成果，worker 会继续完成 SQLite 落地或由其他
   worker 从状态接口修复。

## 资源模型

- 一个 etcd 单节点、一个协调服务、三个 worker 进程。
- 任务始终按 100 条/批在内存中生成和计算，不扫描全量 2000 条历史。
- etcd 只保存 20 个批次的状态与规范成果摘要数据。
- 进程由脚本设置 `GOMAXPROCS=1`，SQLite 每进程最多一个连接，避免超出 2 逻辑 CPU 的并行度。
