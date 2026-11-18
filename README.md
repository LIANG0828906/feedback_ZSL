# order-events 订阅规则与分层位点投递服务

该服务复用本机 Kafka 3.7、Redis 7 和 PostgreSQL 16，为 `order-events`
主题提供订阅组、规则版本、分层位点、切换边界、重复投递审计、积压归因和
Prometheus 指标。服务按单进程、低资源逻辑副本设计，运行时固定为 2 个逻辑
CPU 和 512MiB 最大 Java 堆。

## 关键不变量

- Kafka 位点均为**闭区间语义**：`-1` 表示尚未推进，`confirmedOffset=42`
  表示该分区 42 及之前的事件已经处理完成。
- 拉取层 `readOffset` 只记录已经扫描到的最大 Kafka 位点；确认层
  `confirmedOffset` 只记录消费者确认命中的最大位点。规则排除会推进扫描
  位点，但不会直接推进确认位点。
- 每个分区单独记录规则切换边界。`boundaryOffset` 是**第一条使用新版本
  的事件位点**；`offset < boundaryOffset` 使用旧版本，
  `offset >= boundaryOffset` 使用新版本。
- 拉取遇到最早未确认事件时停止越过它。未确认事件在
  `eventbus.confirm-timeout` 内不会再次投递，超时后允许重投并累加
  `delivery_count`。
- 确认必须严格推进最早一个已投递未确认事件。已确认位点永不回退。
- 归因查询只读取 Kafka、PostgreSQL 和规则快照，不写位点、规则或审计数据。

## 本地启动

前提：JDK 21、Maven 3.9、Kafka 3.7 KRaft 单节点、Redis 7、
PostgreSQL 16，数据库 `eventbus` 与主题 `order-events` 已创建。

```bash
export DB_URL='jdbc:postgresql://localhost:5432/eventbus'
export DB_USERNAME='postgres'
export DB_PASSWORD='postgres'
export KAFKA_BOOTSTRAP_SERVERS='localhost:9092'
export REDIS_HOST='localhost'
export REDIS_PORT='6379'
export SERVER_PORT='8080'
export CONFIRM_TIMEOUT='PT60S'

mvn test
mvn -DskipTests package
scripts/run.sh
```

数据库表由 `src/main/resources/schema.sql` 幂等创建。Redis 是可重建的
热数据镜像，PostgreSQL 是规则、切换记录、确认位点和投递审计的持久化事实源。

主题不存在时可执行：

```bash
export KAFKA_HOME='/path/to/kafka_2.13-3.7.1'
scripts/create_topic.sh
```

## 固定种子

种子使用显式分区写入：订单序号对 6 取模决定分区，每 33 条事件复制一条
同 `eventId` 的重复 Kafka 记录。默认生成 10000 条逻辑事件和约 3% 重复记录。

```bash
scripts/seed_events.sh 10000 33
scripts/bootstrap_subscriptions.sh
```

种子事件字段为 `eventId`、`eventType`、`orderId`、`sourceService`、
`occurredAt`、`payload`。事件类型固定为：

| 事件类型 | 来源服务 |
| --- | --- |
| `ORDER_CREATED` | `order-service` |
| `ORDER_PAID` | `payment-service` |
| `ORDER_SHIPPED` | `fulfillment-service` |
| `REFUND_REQUESTED` | `payment-service` |
| `RETURN_REQUESTED` | `after-sales-service` |

## 规则 DSL

规则由事件类型集合和载荷字段条件组成，所有条件按 `AND` 组合。

```json
{
  "eventTypes": ["ORDER_PAID", "ORDER_SHIPPED"],
  "conditions": [
    {"field": "payload.channel", "operator": "in", "value": ["APP", "WEB"]},
    {"field": "payload.amount", "operator": "gte", "value": 100}
  ]
}
```

支持的操作符：`eq`、`ne`、`gt`、`gte`、`lt`、`lte`、`in`、`contains`、
`exists`、`prefix`、`suffix`、`regex`。字段路径采用点号，例如
`payload.region`；事件类型兼容 `eventType`、`event_type`、`type`，载荷字段
缺省时 `payload.foo` 也会回退读取根字段 `foo`。

## API

### 订阅组与规则

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/subscriptions` | 创建订阅组及规则版本 1 |
| `GET` | `/api/subscriptions` | 查询全部订阅组 |
| `GET` | `/api/subscriptions/{group}` | 查询规则和当前生效版本 |
| `PUT` | `/api/subscriptions/{group}/rule` | 提交完整新规则版本 |
| `POST` | `/api/subscriptions/{group}/rules` | 更新规则别名 |

创建示例：

```bash
curl -sS -X POST http://localhost:8080/api/subscriptions \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "after-sales",
    "rule": {
      "eventTypes": ["REFUND_REQUESTED", "RETURN_REQUESTED"],
      "conditions": []
    }
  }'
```

请求也接受顶层 `eventTypes` 和 `conditions`，不要求嵌套在 `rule` 中。
同一订阅组的更新先获取组级公平锁，并为每次提交分配单调版本号。较低版本
在看到更高生效版本后返回 `409 RULE_UPDATE_STALE`，不会覆盖新版本；响应
中同时给出当时 `currentRuleVersion`。

### 拉取与确认

```bash
curl -sS -X POST http://localhost:8080/api/subscriptions/after-sales/pull \
  -H 'Content-Type: application/json' \
  -d '{"partitions":[0,1,2,3,4,5],"maxEventsPerPartition":100}'

curl -sS -X POST http://localhost:8080/api/subscriptions/after-sales/confirmations \
  -H 'Content-Type: application/json' \
  -d '{"partition":0,"offset":42}'
```

拉取响应按分区返回，`events` 按位点升序；被规则排除的事件不出现在
`events` 中，但会计入本次 `excluded`。有命中事件时扫描会继续到命中或分区
末尾，不会因为前面的排除事件返回空结果。

确认失败码稳定且可与规则错误区分：

| 业务码 | HTTP | 含义 |
| --- | --- | --- |
| `OFFSET_OUT_OF_ORDER` | 409 | 更早的已投递事件仍未确认 |
| `OFFSET_DUPLICATE` | 409 | 位点已经确认 |
| `OFFSET_ROLLBACK` | 409 | 位点低于已确认位点 |
| `OFFSET_NOT_DELIVERED` | 409 | 该位点未被此订阅组投递 |
| `RULE_INVALID` | 400 | 规则 DSL 无效 |
| `RULE_UPDATE_STALE` | 409 | 旧规则提交未覆盖新版本 |
| `SUBSCRIPTION_NOT_FOUND` | 404 | 订阅组不存在 |

### 状态、边界与事件依据

```bash
curl -sS http://localhost:8080/api/subscriptions/after-sales/status
curl -sS http://localhost:8080/api/subscriptions/after-sales/partitions/2/events/1710/decision
curl -sS 'http://localhost:8080/api/subscriptions/notification/deliveries?eventId=EVT-00000015'
```

状态响应包含当前规则版本、每分区两层位点、最新位点、积压量、重复投递次数
和完整的规则切换记录。事件依据接口返回该事件使用的 `ruleVersion`、
`RULE_MATCH`/`RULE_EXCLUDED`/`INVALID_EVENT` 判定及累计投递次数。

### 积压归因

```bash
curl -sS 'http://localhost:8080/api/subscriptions/after-sales/attribution?partitions=0,1,2'
```

归因从 `confirmedOffset + 1` 扫描到分区最新位点，并满足：

```text
gap = ruleExcluded + unconfirmed + redelivery
```

- `ruleExcluded`：按该位点有效规则版本未命中的事件数。
- `unconfirmed`：命中但尚未确认的事件数，包含已读未确认和尚未读到的命中事件。
- `redelivery`：`delivery_count > 1` 的事件数。

`balanced` 表示三项之和与 `gap` 完全对账。归因接口不推进任何位点，也不写
规则或投递审计。`/backlog` 是同一接口的别名。

### 指标

`/metrics`、`/prometheus`、`/actuator/prometheus` 均返回 Prometheus
文本格式。标签固定为 `subscription_group` 和 `partition`：

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `eventbus_pull_total` | counter | 拉取次数 |
| `eventbus_hit_total` | counter | 返回事件数，含重投 |
| `eventbus_excluded_total` | counter | 规则排除事件数 |
| `eventbus_redelivery_total` | counter | 超时重投次数 |
| `eventbus_confirmed_offset` | gauge | 已确认位点 |
| `eventbus_backlog_events` | gauge | 已确认位点之后的积压条数 |
| `eventbus_rule_version` | gauge | 当前规则版本 |

## 存储模型

PostgreSQL 表：

- `subscription_group`、`subscription_rule`：订阅组和所有历史规则版本。
- `rule_switch`：订阅组、分区、旧版本、新版本和切换位点。
- `partition_progress`：每分区可解释的 `read_offset` 与 `confirmed_offset`。
- `event_delivery`：未确认/已确认投递审计及累计投递次数。
- `event_decision`：扫描事件的规则版本和命中/排除原因。
- `delivery_metric`：持久化 Prometheus 计数器。

Redis 键：

- `eventbus:subscription:{group}:meta`：当前规则版本、规则 JSON、更新时间。
- `eventbus:subscription:{group}:progress`：每分区两层位点。

Redis 首次写入失败后会进入 60 秒短路，避免故障时每个分区都等待超时；
数据库不受影响，后续成功操作会重新刷新镜像。

## 故障与重启行为

- **消费者停止/延迟确认**：未确认事件在超时前不重投，超时后从同一个
  已投递位点重投，`deliveryCount` 和重复投递审计增加。
- **服务进程重启**：规则、切换边界、两层位点、待确认事件和重复次数均从
  PostgreSQL 恢复。已确认事件不会重投，超过超时时间的未确认事件会重投。
- **规则切换**：更新请求在同一订阅组锁内采样每个分区的日志末端位点并写入
  `rule_switch`，旧规则在切换前继续生效，新规则从边界位点生效。
- **Redis 中断**：服务继续使用 PostgreSQL，Redis 镜像恢复后由后续请求刷新。
- **PostgreSQL 中断**：依赖持久状态的接口返回 `503 STORAGE_UNAVAILABLE`。
- **Kafka 中断**：需要位点采样的接口返回 `503 KAFKA_UNAVAILABLE`；状态查询
  仍返回数据库部分，实时最新位点字段为 `null`，指标中的积压值输出 `-1`
  表示未知。

## 验证

可选 smoke test 会创建一个临时订阅组，验证拉取、乱序拒绝、连续确认和归因对账：

```bash
BASE_URL=http://localhost:8080 scripts/smoke_test.sh
```

人工验证建议：

1. 拉取并确认分区 0 的前 N 个命中事件，再请求更高位点确认，应得到
   `OFFSET_OUT_OF_ORDER`。
2. 保留一个事件不确认并等待超过 `CONFIRM_TIMEOUT`，重复拉取应看到同一
   位点且 `deliveryCount` 增加。
3. 重启服务后查询状态，两层位点和重复次数保持不变；再次拉取从已确认位点
   之后开始。
4. 更新规则后查询 `/status` 的切换记录，分别回查边界前、边界后事件，应
   看到旧、新 `ruleVersion`。
5. 对任一订阅组查询 `/attribution`，确认所有分区 `balanced=true`。

## 资源与部署边界

- `scripts/run.sh` 设置 `-Xmx512m`、2 个逻辑 CPU、串行写 Redis 镜像，并复用
  单个同步 Kafka consumer，适配 1.25GiB 主机。
- 本实现把生产多节点消费者缩成单进程逻辑副本，组级公平锁保证同组拉取、
  确认和规则更新互斥。
- 若扩展成真实多进程副本，需要把组级锁升级为 PostgreSQL advisory lock 或
  分区所有权租约；当前拓扑按题目约束不启动重复重量级服务。
