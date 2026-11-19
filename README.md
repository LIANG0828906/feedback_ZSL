# driftconverge

面向共享单节点 kind 集群的 GitOps 漂移收敛服务。服务从本地 Git bare 仓库读取
`cluster-dev` 与 `cluster-prod` 两个分支的 Deployment 快照，校验清单与 dev 分支策略，
比较两个隔离 Namespace 的实际状态，按工作负载串行应用，并把整轮审计记录以单个
PostgreSQL 事务提交。

## 快速开始

前置条件：

- Go 1.22
- Git
- Docker Desktop + WSL2 后端
- kind v0.25、kubectl
- PostgreSQL 16，监听 `127.0.0.1:5432`

建议在 WSL2 的 Ubuntu 工作目录中执行脚本。默认数据库名是 `drift315`，默认角色是
`drift`；可用 `DRIFT_PGUSER`、`DRIFT_PGDATABASE`、`DRIFT_DATABASE_URL` 覆盖。

```bash
# 初始化固定种子 315 的 bare 仓库、两个分支、初始 Deployment、kind 集群和数据库
make init

# 编译并启动周期服务
make build
export KUBECONFIG="$PWD/lab/kind.kubeconfig"
./lab/driftd

# 另一个终端查询状态和指标
curl -s localhost:8080/api/v1/clusters/cluster-prod/status
curl -s localhost:8080/metrics
```

单轮运行：

```bash
DRIFT_KUBECONFIG_DEV=/path/to/dev.kubeconfig \
DRIFT_KUBECONFIG_PROD=/path/to/prod.kubeconfig \
./lab/driftd --once
```

`--once` 只在事务提交成功时以 0 退出。业务拒绝（`E_MANIFEST`、`E_POLICY`、`E_WAIT`）
仍会形成已提交轮次并输出完整 JSON 报告；数据库或事务失败时以非 0 退出。

完整确定性场景：

```bash
make scenario
```

报告写入 `artifacts/scenario-report.json`，逐项包含 baseline、策略越界、环境变量
冲突修复、滚动更新阻塞、不可观测保留、重启和 API/指标校验结果。

## 目录与数据流

```text
cmd/driftd/                 服务入口
cmd/reportmerge/            场景 JSON 合并与验收
internal/gitsource/         按分支和提交读取本地 bare 仓库
internal/manifest/          YAML 解析与 E_MANIFEST 校验
internal/policy/            dev -> prod 字段策略与 E_POLICY
internal/compare/           期望/实际差异和收敛判定
internal/kube/              Kubernetes 读取、准入等待与更新
internal/engine/            单轮编排、单资源单次应用
internal/store/             PostgreSQL 事务、追加写与查询
internal/metrics/           Prometheus 文本指标
internal/api/               JSON 状态、健康检查和手动触发
scripts/                    固定种子初始化、故障注入、场景与验证
```

每轮流程：

1. 用 `git rev-parse` 固定两个分支的提交，再用 `git show <commit>:<path>` 读取快照。
2. 解析 Namespace、Deployment 与 `BranchPolicy`，构建资源键
   `<cluster>/<namespace>/<deployment>`。
3. 对 prod 快照执行 dev 分支策略；越界时整份 prod 快照拒绝，不做过滤后应用。
4. 分别访问两个 Namespace；API 失败只影响该集群，并保留它最近一次成功比较的摘要。
5. 对每个资源先等待当前滚动更新稳定，再执行一次更新；同一轮同一资源键不重复应用。
6. 重新读取实际状态，标记每个差异为 `converged`、`unresolved` 或 `blocked`。
7. 将轮次、集群摘要、差异、策略拦截、应用结果和结论放在同一事务中提交。

## 仓库约定

仓库必须包含 `DRIFT_MANIFEST_PATH`（默认 `manifests`）下的 YAML 文件。

Deployment 必须显式声明：

- `apiVersion: apps/v1`、`kind: Deployment`
- `metadata.name`、`metadata.namespace`
- `spec.replicas`
- `spec.selector.matchLabels`
- `spec.template.metadata.labels`
- 非空 `spec.template.spec.containers`
- 每个容器的 `name`、`image`
- 每个 env 项的 `name` 和字面量 `value`

同一快照必须声明 Deployment 引用的 Namespace：

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: cluster-prod
```

以下情况整份快照返回 `E_MANIFEST`，错误对象包含 `resource_key` 与字段：

- YAML 无法解析
- 同一资源键重复
- 必填字段缺失或类型错误
- 容器名或 env 键重复
- Deployment 引用了快照内未声明的 Namespace
- Deployment 的 Namespace 与配置的目标 Namespace 不一致（防止跨逻辑集群写入）
- 快照内没有 Deployment

`env.valueFrom` 不参与确定性比较，因此会被 `E_MANIFEST` 拒绝；本服务只比较字面量
环境变量值。

## 分支策略

dev 分支可以包含一个非 Deployment 的策略文档。该文档只用于策略计算，不会被
`kubectl apply`：

```yaml
apiVersion: driftconverge/v1
kind: BranchPolicy
metadata:
  name: dev-to-prod-guard
spec:
  prodReplicaCeiling: 2
  prodForbiddenImageTags:
    - "3.9-debug"
    - "*-dev"
  prodForbiddenEnvKeys:
    - DEBUG_MODE
```

规则：

- prod 使用 dev-only 镜像标签时，整份 prod 快照拒绝。
- prod 出现 dev-only 环境变量键时，整份 prod 快照拒绝。
- prod 副本数超过 dev 声明的上限时，整份 prod 快照拒绝。
- 支持精确标签和 `path.Match` 风格通配符。
- 未配置显式策略时，服务还会把“只在 dev 出现的标签/环境变量键”和 dev 最大副本数
  作为保守的推断策略；显式策略用于在错误合并后仍能识别原本仅供 dev 使用的字段。

所有越界项都写入 `policy_blocks`，包含 `resource_key`、`field`、`value`、
`source_branch` 和 `declared_in_branch`。业务错误码为 `E_POLICY`，与
`E_MANIFEST` 可区分。

## 准入、应用与收敛

存在差异的资源在应用前会检查当前 Deployment：

- `observedGeneration` 未追上 `generation`
- 存在 `Progressing=False` 或 `ProgressDeadlineExceeded`
- 有不可用副本
- 更新副本或可用副本少于当前期望副本
- Pod 数尚未收敛到当前期望副本

出现上述情况时，服务轮询等待，不修改 Deployment。超过
`DRIFT_ADMISSION_TIMEOUT` 后，该资源的差异全部标记为 `blocked`，应用结果记录
`E_WAIT`，集群结论为 `partial`。服务不会强制覆盖，也不会回退到历史快照。

稳定后每个资源只更新一次。更新只变更本次收敛范围的 `replicas`、容器 `image` 和
容器 env；不会删除清单之外的 sidecar 容器。更新后重新列出 Namespace 的实际
Deployment 并计算：

- `converged`：所有差异都已消失
- `partial`：有阻塞项或仍存在未收敛字段
- `failed`：清单/策略/可观测性失败，或至少一次应用返回 Kubernetes 错误

失败信息保留在 `apply_results[].message` 与集群摘要的 `error_message` 中。

## 持久化与恢复

默认连接：

```text
postgres://drift@127.0.0.1:5432/drift315?sslmode=disable
```

表：

- `drift_rounds`
- `drift_round_clusters`
- `drift_differences`
- `drift_policy_blocks`
- `drift_apply_results`

一致性保证：

- 一轮的所有记录在 `SERIALIZABLE` 事务中提交；任一行写入失败则整轮回滚。
- 轮次只在完整结果生成后出现，进程崩溃留下的未提交事务不会成为“已收敛”。
- `round_id` 是主键；已提交 ID 再次写入返回唯一冲突，不会覆盖结论。
- 五张表都有阻止 `UPDATE`/`DELETE` 的触发器，已提交记录保持追加写。
- 重启时服务从数据库读取每集群最新状态以初始化指标；不可观测轮次通过
  `last_observed_round_id` 引用最近一次 `comparison_success=true` 的轮次。

`observable` 只描述本轮 Kubernetes API 是否可访问；`comparison_success` 描述本轮
是否真正完成了实际状态比较。清单或策略失败可以是 `observable=true`，但绝不会被
当作“最近一次成功比较”。

## JSON API

状态查询：

```text
GET /api/v1/clusters/{cluster}/status
GET /api/v1/status?cluster={cluster}
```

`cluster` 取值为 `cluster-dev` 或 `cluster-prod`。返回最近一次已提交轮次：

```json
{
  "round_id": "round-...",
  "cluster": "cluster-prod",
  "branch": "cluster-prod",
  "git_commit": "...",
  "committed_at": "2026-11-19T06:00:00Z",
  "observable": true,
  "comparison_success": true,
  "expected_count": 2,
  "diff_count": 1,
  "policy_block_count": 0,
  "blocked_count": 0,
  "converged_resources": 2,
  "convergence": "converged",
  "error_code": "",
  "error_message": "",
  "error_resource_key": "",
  "error_field": "",
  "error_source_branch": "",
  "last_observed_round_id": "",
  "differences": [],
  "policy_blocks": [],
  "apply_results": []
}
```

差异数组使用稳定排序 `(resource_key, field, component, env_key)`。每条差异包含
`expected` 与 `actual`，字段值为：

- `exists`
- `replicas`
- `image`，`component` 为容器名
- `env`，`component` 为容器名，`env_key` 为环境变量键

其他端点：

```text
GET  /healthz
GET  /metrics
POST /api/v1/reconcile
```

`POST /api/v1/reconcile` 同步执行一轮；已有轮次运行时返回 HTTP 409 和 `E_BUSY`。

## Prometheus 指标

指标名称和标签固定：

```text
drift_expected_resources{cluster="cluster-dev"}
drift_differences{cluster="cluster-dev"}
drift_policy_blocks{cluster="cluster-dev"}
drift_blocked_items{cluster="cluster-dev"}
drift_converged_resources{cluster="cluster-dev"}
drift_unobservable{cluster="cluster-dev"}
```

每项都是 gauge，并为两个集群各输出一条序列。`drift_differences` 在不可观测轮次中
继续使用最近一次成功比较的摘要；`drift_unobservable` 为 1 时不能把差异数解释为 0。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIFT_GIT_REPO` | `./lab/repo.git` | 本地 bare 仓库 |
| `DRIFT_MANIFEST_PATH` | `manifests` | 快照 YAML 子目录 |
| `DRIFT_BRANCH_DEV` | `cluster-dev` | dev 分支 |
| `DRIFT_BRANCH_PROD` | `cluster-prod` | prod 分支 |
| `DRIFT_NAMESPACE_DEV` | `cluster-dev` | dev Namespace |
| `DRIFT_NAMESPACE_PROD` | `cluster-prod` | prod Namespace |
| `DRIFT_KUBECONFIG_DEV` | 默认 kubeconfig | dev 集群配置 |
| `DRIFT_KUBECONFIG_PROD` | 默认 kubeconfig | prod 集群配置 |
| `DRIFT_CLUSTER_TIMEOUT` | `5s` | 单次 Kubernetes API 请求时限 |
| `DRIFT_DATABASE_URL` | `postgres://drift@127.0.0.1:5432/drift315?sslmode=disable` | PostgreSQL |
| `DRIFT_HTTP_ADDR` | `:8080` | API 与指标监听地址 |
| `DRIFT_INTERVAL` | `5s` | 周期间隔 |
| `DRIFT_ADMISSION_TIMEOUT` | `20s` | 准入等待上限 |
| `DRIFT_ADMISSION_POLL` | `1s` | 准入轮询间隔 |
| `DRIFT_SEED` | `315` | 初始化样本种子 |
| `DRIFT_KIND_CLUSTER` | `drift-lab` | kind 集群名 |
| `DRIFT_LAB_DIR` | `./lab` | 仓库、kubeconfig 与二进制目录 |
| `DRIFT_SCENARIO_PORT` | `18315` | 场景 HTTP 校验端口 |

`cluster-dev` 与 `cluster-prod` 可以通过不同的 kubeconfig 指向同一 kind 集群的
不同 Namespace，也可以分别指向独立集群，无需修改源码。

## 故障注入与场景

初始化：

```bash
scripts/init_lab.sh --force
```

注入命令：

```bash
scripts/inject.sh reset
scripts/inject.sh policy          # dev-only 镜像标签、env 键和副本上限进入 prod 快照
scripts/inject.sh env-conflict    # 直接改 prod Deployment 的 APP_MODE
scripts/inject.sh stuck-rollout   # prod 使用不可拉取镜像，制造未完成滚动更新
scripts/inject.sh api-unreachable # 生成 prod 不可达 kubeconfig
```

这些命令只改 Git 分支、Kubernetes 对象或 kubeconfig，不改服务源码。策略注入会同时
修改 dev 和 prod 分支，使“错误合并后标签在两个分支都出现”的边界仍能被显式 dev
策略识别。

`scripts/scenario.sh` 会依次执行并验证：

1. baseline 两集群收敛
2. prod 越界快照被 `E_POLICY` 整份拒绝
3. prod 环境变量漂移被检测并修复
4. stuck rollout 超时后标记 `E_WAIT`，不应用
5. 制造可观测差异后，用坏 kubeconfig 使 prod 不可观测，验证摘要和轮次 ID 保留
6. 新进程重启后继续从已提交轮次运行
7. HTTP JSON 状态与全部 Prometheus 序列存在

最终报告中的 `passed` 为场景验收总结果，`checks` 是逐项可审计断言。

## 测试

```bash
go test ./...

DRIFT_PGDATABASE=drift315_test scripts/init_db.sh
TEST_DATABASE_URL='postgres://drift@127.0.0.1:5432/drift315_test?sslmode=disable' \
  go test ./internal/store -v

scripts/verify.sh
```

单元测试覆盖清单重复键、未声明 Namespace、字段缺失、策略字段识别、差异期望/实际值、
准入超时、不可观测摘要保留和单资源单次应用。PostgreSQL 集成测试覆盖事务回滚、
提交后不可变、同轮次 ID 防覆盖和重启后记录一致。

## 资源边界

- 单个 kind 控制平面；不增加节点或副本控制器之外的逻辑实例。
- 两个 Namespace 共用同一个 API Server。
- 样本只有 5 个 `pause:3.9` Pod（dev 3 + prod 2），避免额外镜像和内存占用。
- 服务顺序处理资源；Kubernetes 客户端限制为 QPS 5/Burst 10。
- PostgreSQL 连接池最多 4 条连接；场景使用独立数据库 `drift315`。
- 初始化、注入和报告只使用小型 YAML/JSON 文件，不生成大体积临时数据。
