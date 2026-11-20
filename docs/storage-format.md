# 存储格式与一致性协议

## 目录布局

```text
<data>/
  manifest.ekv
  snapshots/
    snapshot-<generation>.esnap
  logs/
    log-<generation>.elog
    archive/
      log-<generation>.elog
  repair/
    backup-001/
      root/
      report.json
  reports/
    recovery.json
```

`manifest.ekv` 是唯一的提交指针。快照和日志均带 `chainId` 与 `generation`；只有清单引用的组合属于活动数据。

## 清单格式

清单为 UTF-8 文本：

```text
EDGEKV_MANIFEST_V1
formatVersion=1
chainId=edgekv-local-chain
generation=2
snapshotFile=snapshots/snapshot-2.esnap
snapshotVersion=5
minLiveVersion=1
logFile=logs/log-2.elog
checksum=<SHA-256>
```

`checksum` 覆盖前七行的字段名、等号、值和换行。更新时先写 `manifest.ekv.tmp` 并 `fsync`，然后使用原子移动替换清单，最后对目录执行 `fsync`。

## 追加日志

### 文件头

```text
int32   magic = 0x454B4C31 ("EKL1")
uint8   format = 1
utf8    chainId
int64   generation
uint32  CRC32(以上所有文件头字节)
```

文件头通过临时文件加原子移动创建，不允许活动日志出现半个头部。

### 记录帧

```text
int32   magic = 0x454B5231 ("EKR1")
int32   payloadLength
bytes   payload
uint32  CRC32(payload)
```

payload：

```text
uint8   operation (1=PUT, 2=DELETE)
int64   version
int64   expireAt (>0 为显式逻辑过期时刻，0 为不过期)
utf8    key
utf8    value
```

### 尾部判定

恢复从文件头之后逐帧扫描：

- 剩余字节不足 4、或存在合法帧魔数但头部/负载不足完整帧：视为可安全跳过的部分写入，记录 `skippedTruncatedBytes`，重放完整记录后截断残余。
- 完整帧魔数不符合、长度非法、完整帧 CRC 错误、payload 不可解码：视为不可安全忽略，抛出 `E_LOG_TRUNCATED`，不自动修改文件。
- 版本必须严格递增，且必须大于快照版本；否则视为快照/日志链冲突，抛出 `E_SNAPSHOT_VERSION_CONFLICT`。

## 快照

```text
int32   magic = 0x454B5331 ("EKS1")
uint8   format = 1
utf8    chainId
int64   generation
int64   snapshotVersion
int64   minLiveVersion
int32   payloadLength
bytes   SHA-256(payload)
bytes   SHA-256(以上头部字段)
bytes   payload
```

payload：

```text
int32   entryCount
repeat:
  utf8  key
  utf8  value
  int64 version
  int64 expireAt
```

`snapshotVersion` 是压缩时的全局最大版本。`minLiveVersion` 是快照中存活记录的最小版本；快照为空时等于 `snapshotVersion`。删除墓碑和按注入时钟已过期的键不进入快照。

加载时同时验证：

1. 头部摘要；
2. 负载 SHA-256；
3. `chainId`、`generation`、`snapshotVersion`、`minLiveVersion` 与清单一致；
4. 对应日志头部的 `chainId` 和 `generation` 一致。

任一失败均不会删除旧文件，启动停止并进入显式修复。

## 压缩协议

1. 在当前锁内获取 `snapshotVersion` 和注入时钟下的存活集合。
2. 写入 `snapshot-<next>.esnap.pending` 并通过完整反序列化校验。
3. 原子移动为正式快照文件，`fsync` 快照目录。
4. 触发 `after-snapshot-write` 故障点。
5. 创建带新 `generation` 的新空日志文件。
6. 原子发布指向新快照和新日志的清单，`fsync` 根目录。
7. 触发 `after-snapshot-replace` 故障点。
8. 关闭旧日志句柄，将旧日志移动到 `logs/archive/`。
9. 触发 `after-log-truncate` 故障点。

清单是跨文件事务的提交点。因此任意终止位置只会产生：

- 旧清单：旧快照 + 完整旧日志；或
- 新清单：新快照 + 新空日志。

新日志始终在清单提交前创建，旧日志始终在清单提交后轮换，因而不存在“新清单指向不存在日志”或“旧日志被提前截断”的窗口。

## 恢复流程

1. 读取并校验清单。
2. 加载清单引用的快照，建立键表和基准版本。
3. 校验活动日志身份。
4. 从头解析活动日志；完整帧加入重放序列，安全尾部残余记录字节数。
5. 对版本大于快照版本的记录按键覆盖写入；DELETE 记录生成墓碑。
6. 全部验证成功后，才截断安全尾部残余。
7. 打开追加通道，暴露恢复摘要：
   - `replayedRecords`
   - `skippedTruncatedBytes`
   - `finalVersion`
   - `snapshotVersion`
   - `minLiveVersion`
   - 活动日志记录数和字节数

## 修复策略

错误检测只做诊断，不在普通启动路径中删除历史。

### `E_LOG_TRUNCATED`

`repair` 先复制完整数据目录，再将活动日志截断到最后完整帧偏移。完整帧之后的损坏内容不会继续参与版本链，原始文件可从备份审查。

### `E_SNAPSHOT_VERSION_CONFLICT`

`repair` 依次尝试：

1. 扫描所有完整有效的快照，寻找 `chainId` 匹配、对应日志可读且无陈旧版本记录的“快照 + 日志”组合；优先选择最高 `snapshotVersion`，其次最高代际。
2. 若没有可用快照，扫描活动及归档日志，寻找从版本 1 开始且连续递增的完整基础日志。
3. 从基础日志复制记录到新代际日志文件，校验后发布无快照的新清单。
4. 若两条路径均不可行，修复失败并保留备份，不静默清空数据。

## 并发与边界

当前实现定位为单进程本地索引服务，通过 `synchronized` 串行化写操作、查询和压缩。部署时不应由多个进程同时打开同一数据目录。数值时钟、键和值均由调用方提供；过期逻辑不依赖系统时间或延迟。
