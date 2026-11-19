# 验证记录

验证日期：2026-11-19。测试后端为从 `RELEASE.2024-01-16T16-07-38Z` 标签源码构建的 MinIO 单进程服务，监听 `127.0.0.1:9000`。网关为 Linux amd64 测试构建；同一代码已交叉编译为 Windows amd64 二进制。

## 确定性负载

```text
fixtures=120 objects=120 incomplete_sessions=80
object_bytes=99493959
incomplete_bytes=12253204
small_objects=90
multipart_objects=30
v2_requests=121
v4_requests=222
```

121 个 V2 请求由 1 个建桶、90 个小对象 PUT 与 30 个分片 Complete 组成，其余分片控制与 12 个覆盖写为 V4。

装载并逐版本校验：

```json
{
  "listed_versions": 132,
  "objects": 120,
  "sessions": 80,
  "status": {
    "bucket": "s3gw-samples",
    "current_versions": 120,
    "has_incomplete_sessions": true,
    "historical_versions": 12,
    "oldest_incomplete_session_age_seconds": 7279,
    "recent_reclaims": []
  },
  "versions_verified": 132
}
```

每个首个版本均按原始文件 SHA-256 与长度回读校验；12 个 V4 覆盖版本按当前版本响应校验；ListObjectVersions 返回数一致。

## 生命周期回收

将同一 SQLite 状态以 `part-retention=1h` 重启后，80 个 2 小时前的会话在启动回收中完成：

```text
reaper sessions=80 reclaimed=80 failed=0 bytes=12253204
```

回收耗时约 `145ms`。随后再次执行 `-verify-only`：

```text
listed_versions=132
current_versions=120
historical_versions=12
has_incomplete_sessions=false
multipart_sessions_incomplete=0
multipart_bytes_incomplete=0
reaper_bytes_reclaimed_total=12253204
reaper_failures_total=0
```

120 个当前对象与 132 个版本仍全部可读取，说明回收未触碰已提交对象。

并发压力下的回收边界：另生成 80 个 2 小时前会话（14,120,572 字节）到 `s3gw-bench`，在 16 并发、400 次 256KiB PUT/GET 压测进行中触发手动回收：

```json
{"examined_sessions":80,"reclaimed_sessions":80,"failed_sessions":0,"reclaimed_bytes":14120572}
```

本次回收耗时约 `536ms`。压测错误数 `0/400`，错误率 `0%`；PUT P95 `32.5ms`、GET P95 `25.2ms`、4MiB 首次 GET `15.1ms`。

## 独立 SDK 互操作

使用 `minio-go/v7 v7.0.66` 直接连接网关：

- `credentials.NewStaticV4`：PUT、GET、Stat、Remove、显式 Initiate/UploadPart/Complete 全部成功。
- `credentials.NewStaticV2`：PUT 与 GET 成功。
- SigV4 默认流式上传路径 `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` 被逐块验证并正确解码。
- 4MiB 显式分片对象返回 multipart ETag `...-2`，回读内容一致。

## 性能

网关和 MinIO 进程均通过 `taskset` 限制到 2 个逻辑 CPU；客户端在另外两个逻辑 CPU 上运行。200 个 256KiB 对象、并发 8：

```json
{
  "get": {
    "errors": 0,
    "p50_ms": 4.755,
    "p95_ms": 8.645,
    "p99_ms": 10.916
  },
  "multipart": {
    "complete_ms": 48.507,
    "first_get_bytes": 4194304,
    "first_get_ms": 10.958,
    "first_get_status": 200,
    "status": 200,
    "total_ms": 116.991,
    "version_id": "v00000000000000000342"
  },
  "put": {
    "errors": 0,
    "p50_ms": 12.636,
    "p95_ms": 19.794,
    "p99_ms": 22.52
  }
}
```

结果满足 PUT P95 小于 150ms、GET P95 小于 80ms、4MiB Complete 后首次 GET 小于 2s。

## 自动测试

```text
go test ./...          PASS
go test -race ./...    PASS
go vet ./...           PASS
GOOS=windows GOARCH=amd64 go build ./cmd/s3gw  PASS
```
