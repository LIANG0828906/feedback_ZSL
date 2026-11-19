# S3 版本兼容网关

面向 Windows 10/11 与 Windows Server 2022 的单进程对象存储网关。网关对外提供 HTTP/1.1 S3 API，将对象内容写入本地 MinIO，并用内嵌 SQLite 维护逻辑桶、版本链、删除标记、分片会话与回收账本。

## 核心保证

- 同时校验 Signature V2、Signature V4（含预签名 URL）和 SigV4 `aws-chunked` 流式上传；签名失败、缺少必要头或负载哈希不符的请求不会进入 MinIO。
- 版本 ID 形如 `v00000000000000000042`，来自 SQLite `AUTOINCREMENT`，全局唯一、可按字典序比较，删除后不会复用。
- 每次 PUT 或 Complete 都创建独立版本；旧客户端省略 ACL/存储类别时使用空 ACL 与 `STANDARD`，但仍进入同一版本链。
- 未指定版本的 GET 读取最新提交版本；指定版本读取历史版本；删除标记作为版本参与链排序。
- 未完成分片存储于 `.s3gw/v1/uploads/...`，回收器只扫描该前缀，不触碰 `.s3gw/v1/objects/...` 中已提交对象。
- 所有对象内容与分片均由 MinIO 保存；SQLite 只保存索引与账本，不会出现绝对路径。

## 目录

```text
cmd/s3gw/         网关与后台回收器
cmd/samplegen/    固定种子样本生成、装载、校验工具
cmd/s3gwbench/    带签名的小对象与 4MiB 分片性能测试
internal/sig      SigV2/SigV4、预签名、aws-chunked 校验与签名工具
internal/store    SQLite 模式、事务与查询
internal/backend  MinIO SigV4 客户端
internal/gateway  S3 路由、版本、分片、指标与状态接口
scripts/          Windows PowerShell 构建与启动脚本
```

## Windows 快速启动

要求 Go 1.22+、MinIO `RELEASE.2024-01-16T16-07-38Z` Windows amd64 二进制。

```powershell
cd <仓库目录>
.\scripts\build.ps1

.\scripts\start.ps1 `
  -MinioBinary "D:\tools\minio.exe" `
  -MinioData "D:\s3data" `
  -DataRoot "D:\s3gw"
```

`start.ps1` 会设置 MinIO 默认凭据、等待 `http://127.0.0.1:9000/minio/health/live` 就绪，再启动网关；网关退出时同步停止它启动的 MinIO 子进程。MinIO 已单独运行时加 `-SkipMinio`。

等价的 MinIO 启动命令：

```powershell
$env:MINIO_ROOT_USER = "minioadmin"
$env:MINIO_ROOT_PASSWORD = "minioadmin"
minio.exe server D:\s3data --address 127.0.0.1:9000
```

等价的网关启动命令：

```powershell
.\bin\s3gw.exe `
  -listen 127.0.0.1:9100 `
  -backend http://127.0.0.1:9000 `
  -data D:\s3gw\data\gateway.db `
  -temp-dir D:\s3gw\tmp `
  -part-retention 336h `
  -reaper-interval 1m
```

手动构建：

```powershell
go mod download
$env:GOOS = "windows"; $env:GOARCH = "amd64"
go build -trimpath -o .\bin\s3gw.exe .\cmd\s3gw
go build -trimpath -o .\bin\samplegen.exe .\cmd\samplegen
go build -trimpath -o .\bin\s3gwbench.exe .\cmd\s3gwbench
```

## 配置

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1:9100` | 对外 HTTP/1.1 地址 |
| `-backend` | `http://127.0.0.1:9000` | MinIO S3 地址 |
| `-region` | `us-east-1` | SigV4 区域，必须与客户端一致 |
| `-access-key` | `gateway` | 客户端访问键 |
| `-secret-key` | `gateway-secret` | 客户端密钥 |
| `-backend-access-key` | `minioadmin` | MinIO 访问键 |
| `-backend-secret-key` | `minioadmin` | MinIO 密钥 |
| `-data` | `data/gateway.db` | SQLite 文件 |
| `-temp-dir` | 数据库同目录 `tmp` | 临时目录 |
| `-part-retention` | `336h` | 会话自 Initiate 起的保留时长 |
| `-reaper-interval` | `1m` | 后台回收周期 |
| `-clock-skew` | `15m` | 允许时钟偏差，`0` 关闭检查 |
| `-max-object-bytes` | `67108864` | 单个请求体/对象上限 |
| `-max-list-keys` | `1000` | 列表接口上限 |
| `-disable-reaper` | `false` | 关闭后台回收 |
| `-log-requests` | `false` | 每请求一行日志 |

对应环境变量为 `S3GW_*`，例如 `S3GW_LISTEN`、`S3GW_DATA`、`S3GW_PART_RETENTION`。

## S3 接口

| 方法 | 路径/查询 | 行为 |
|---|---|---|
| `PUT` | `/bucket` | 创建本地与 MinIO 桶 |
| `GET` | `/` | ListBuckets |
| `GET` | `/bucket?list-type=2` | ListObjectsV2 |
| `PUT` | `/bucket/key` | 创建对象新版本 |
| `GET`, `HEAD` | `/bucket/key[?versionId=...]` | 当前或指定历史版本 |
| `DELETE` | `/bucket/key[?versionId=...]` | 删除标记或移除指定版本 |
| `GET` | `/bucket?versions` | ListObjectVersions，按键升序、版本从新到旧 |
| `POST` | `/bucket/key?uploads` | InitiateMultipartUpload |
| `PUT` | `/bucket/key?partNumber=N&uploadId=...` | UploadPart |
| `GET` | `/bucket/key?uploadId=...` | ListParts |
| `POST` | `/bucket/key?uploadId=...` | CompleteMultipartUpload |
| `DELETE` | `/bucket/key?uploadId=...` | AbortMultipartUpload |
| `GET` | `/bucket?uploads` | ListMultipartUploads |

列表与对象 XML 使用 `http://s3.amazonaws.com/doc/2006-03-01/` 命名空间。对象响应包含 `ETag`、`x-amz-version-id`，删除标记响应额外包含 `x-amz-delete-marker: true`。

### 签名兼容

- SigV2：`Authorization: AWS <access>:<base64-hmac-sha1>`。
- SigV4：`Authorization: AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...`。
- SigV4 流式负载：支持 `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`、`...-TRAILER` 与 `STREAMING-UNSIGNED-PAYLOAD-TRAILER` 的 aws-chunked 解码；有签名的块逐块验证。
- 预签名：支持 SigV2 的 `AWSAccessKeyId/Signature/Expires` 与 SigV4 的 `X-Amz-*` 查询参数。
- 匿名请求，包括匿名写，一律拒绝。错误 XML 含 `Code`、`Message`、`RequestId`、`HostId`。

常用稳定错误码：`AccessDenied`、`InvalidAccessKeyId`、`SignatureDoesNotMatch`、`MissingSecurityHeader`、`XAmzContentSHA256Mismatch`、`NoSuchKey`、`NoSuchVersion`、`NoSuchUpload`、`InvalidPart`、`InvalidPartOrder`、`EntityTooLarge`。

## 版本与删除语义

- 提交前的版本状态为 `pending`，不可见；MinIO 写入成功后才原子变为 `committed`。
- 版本按保留顺序编号；并发写同一键不会覆盖，当前版本是最大提交版本 ID。
- `DELETE` 无 `versionId` 时追加删除标记版本；当前 GET 返回 `NoSuchKey`。
- `DELETE?versionId=...` 移除指定版本。该行保留为删除墓碑，因此重复删除返回相同的 `204`，且不会新增删除标记或复用 ID。
- ListObjectVersions 同时包含普通历史版本与删除标记，`IsLatest` 对每个键唯一。

## 分片与回收

- Initiate 时创建会话；UploadPart 将分片写到 `.s3gw/v1/uploads/<uploadID>/parts/NNNNN`，SQLite 记录 ETag、长度与后端键。
- Complete 先校验分片编号、ETag、后端实际长度和 MD5，全部通过后才创建对象版本并原子提交组合清单。
- 校验失败返回 `InvalidPart`，会话恢复为 `active`，可继续上传后再次 Complete。
- Abort 删除该会话所有 MinIO 分片和 SQLite 分片记录。
- 回收器按 `part-retention` 选择 `active` 会话，原子切换为 `reaping`，删除分片，再写入 `reclaim_ledger`。回收期间 Complete 返回 `NoSuchUpload`。

## 运维接口

以下接口无需 S3 签名，默认仅建议绑定回环地址：

- `GET /healthz`
- `GET /metrics`
- `GET /_gateway/status?bucket=<bucket>`
- `POST /_gateway/reap`：立即执行一次回收并返回本次统计

### Prometheus 指标

```text
s3gw_signature_requests_total{signature_version="v2|v4|none",bucket="...",operation="..."}
s3gw_signature_failures_total{signature_version="v2|v4|none",bucket="...",reason="..."}
s3gw_versions_current{bucket="..."}
s3gw_versions_historical{bucket="..."}
s3gw_multipart_sessions_incomplete{bucket="..."}
s3gw_multipart_bytes_incomplete{bucket="..."}
s3gw_reaper_bytes_reclaimed_total{bucket="..."}
s3gw_reaper_bytes_last_run{bucket="..."}
s3gw_reaper_failures_total{bucket="..."}
```

`s3gw_reaper_bytes_*` 只统计生命周期回收，不统计显式 Abort。

### 状态 JSON

字段顺序固定，最近回收记录按时间从新到旧：

```json
{
  "bucket": "s3gw-samples",
  "current_versions": 120,
  "historical_versions": 12,
  "has_incomplete_sessions": false,
  "oldest_incomplete_session_age_seconds": 0,
  "recent_reclaims": [
    {
      "upload_id": "9d0f953cc9a3ee3eb5c0191b07d32374",
      "bytes": 192943,
      "reason": "expired",
      "status": "success",
      "reclaimed_at": "2026-11-19T04:03:22.219068574Z"
    }
  ]
}
```

## 确定性样本

`samplegen` 会根据 `-seed` 生成：

- 90 个 1KiB 到 256KiB 小对象；
- 30 个 1MiB 到 4MiB 分片对象；
- 总计约 100MB 对象数据；
- 80 个带分片字节的未完成会话；
- 90 个 V2 小对象 PUT、30 个 V2 Complete 与 12 个带 ACL/存储类别的 V4 覆盖写。

生成并装载到运行中的网关：

```powershell
.\bin\samplegen.exe `
  -out D:\s3gw\samples `
  -seed 301 `
  -bucket s3gw-samples `
  -endpoint http://127.0.0.1:9100 `
  -data D:\s3gw\data\gateway.db `
  -backend http://127.0.0.1:9000 `
  -session-age 2h `
  -verify
```

只校验已装载数据，不重写：

```powershell
.\bin\samplegen.exe -out D:\s3gw\samples -endpoint http://127.0.0.1:9100 -verify-only
```

`-data` 用于直接写入 80 个回填年龄的会话；如果不需要生命周期样本，可省略该参数。样本目录还包含 `manifest.json`、`sessions.json` 和 `requests.json`。

## 测试

```powershell
go test ./...
go test -race ./...
```

测试覆盖双签名、拒绝未签名写、并发唯一版本、历史版本读取、删除标记幂等、分片补传/Complete/Abort、过期回收、状态 JSON 与指标文本。

性能测试：

```powershell
.\bin\s3gwbench.exe `
  -endpoint http://127.0.0.1:9100 `
  -requests 200 `
  -size 262144 `
  -concurrency 8
```

本仓库在 MinIO `RELEASE.2024-01-16T16-07-38Z` 源码构建的服务上、将网关与 MinIO 进程限制在 2 个逻辑 CPU 后实测：256KiB PUT P95 `19.8ms`，GET P95 `8.6ms`，4MiB 分片 Complete `48.5ms`，Complete 后首次 GET `11.0ms`，错误数 `0`。完整记录见 `docs/RESULTS.md`。

## 明确边界

- 这是单进程网关；SQLite 使用 WAL 且写连接串行化，适合题述约 100MB/120 对象的负载。
- ACL 与存储类别作为版本元数据保存，不实现桶策略、匿名 ACL 或对象锁。
- 不支持服务端加密、复制、生命周期规则 API、对象标签 API 或 Range 下载。
- Range 请求当前返回完整对象；分片最小尺寸不强制 5MiB，以兼容题述 1MiB 到 4MiB 分片。
