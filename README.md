# EdgeKV Index

面向边缘设备本地键值索引的 Java 17 实现。数据由追加日志、校验快照和原子清单指针共同持久化，支持显式逻辑时钟、全局单调版本、删除墓碑、过期查询、压缩、崩溃恢复和显式修复。

## 核心能力

- `PUT`、`GET`、`DELETE`、`expired` 接口，所有写操作分配严格递增的全局版本。
- 过期判断只使用调用方注入的 `--now` 或交互式 `now <epoch>`，不读取系统墙上时钟。
- 日志记录包含操作类型、键、值、逻辑版本、过期时间，并以长度前缀、帧魔数和 CRC32 校验。
- 日志尾部部分写入可被安全截断；完整帧校验失败则触发 `E_LOG_TRUNCATED` 显式修复流程。
- 快照包含 `snapshotVersion`、`minLiveVersion` 和压缩时全部未过期键，并带头部及负载 SHA-256 校验。
- 压缩采用“临时快照 → 校验 → 原子发布快照 → 原子发布清单 → 轮换日志”的顺序。
- 清单通过原子替换在“旧快照 + 完整旧日志”和“新快照 + 新空日志”之间切换，不会产生混合状态或版本回退。
- 四个可注入故障点均通过命令行参数触发，进程以退出码 `137` 强制终止。
- 冲突或不可安全忽略的日志损坏会输出 JSON 修复报告到 `stderr`，并在 `repair/backup-NNN/` 保留原始文件。
- 提供交互式 Shell、单命令 CLI、固定样本和跨平台确定性演示脚本。

## 环境

要求 Java 17+、Maven 3.9+。当前工作区已准备本地 Linux JDK 与 Maven：

```text
.tools/jdk17/usr/lib/jvm/java-17-openjdk-amd64
.tools/maven
.m2repo/                 # Maven 本地依赖缓存，可离线构建
```

脚本会自动检测上述本地工具链；也可以自行设置标准环境变量：

```bash
export JAVA_HOME=/path/to/jdk-17
export PATH="$JAVA_HOME/bin:$PATH"
```

Windows 请安装 JDK 17+ 并设置 `JAVA_HOME`，同时确保 `mvn.cmd` 可执行。项目提供 `build.bat`、`edgekv.bat` 和 `scripts\demo.bat`。

## 构建与测试

Linux/macOS：

```bash
scripts/build.sh test
scripts/build.sh package
```

Windows：

```bat
build.bat test
build.bat package
```

已有本地缓存时，构建脚本自动使用 Maven 离线模式。当前验证环境：

```text
OpenJDK 17.0.21.1
Apache Maven 3.9.9
JUnit Jupiter 5.10.2
```

## 固定样本

`samples/fixed-sample.tsv` 使用固定逻辑时钟 `1700000100`，同时包含普通键、过期键和删除墓碑：

```text
now=1700000100
PUT     stable-node     value-stable    0
PUT     session:old     token-old       1700000000
PUT     doomed-node     value-doomed    0
DELETE  doomed-node     0
PUT     session:fresh   token-new       1700000200
```

预期初始摘要：

```json
{
  "finalVersion": 5,
  "counts": {"live": 2, "expired": 1, "deleted": 1},
  "keys": [
    {"key": "doomed-node", "status": "DELETED", "version": 4},
    {"key": "session:fresh", "status": "LIVE", "version": 5},
    {"key": "session:old", "status": "EXPIRED", "version": 2},
    {"key": "stable-node", "status": "LIVE", "version": 1}
  ]
}
```

## 命令行

Linux：

```bash
scripts/run.sh reset --data .runtime/demo-store
scripts/run.sh sample --data .runtime/demo-store
scripts/run.sh summary --data .runtime/demo-store --now 1700000100
scripts/run.sh get --data .runtime/demo-store --key session:old --now 1700000100
scripts/run.sh expired --data .runtime/demo-store --now 1700000100
scripts/run.sh put --data .runtime/demo-store --key alpha --value one --expire-at 0
scripts/run.sh delete --data .runtime/demo-store --key alpha
scripts/run.sh compact --data .runtime/demo-store --now 1700000100
scripts/run.sh report --data .runtime/demo-store
scripts/run.sh export-report --data .runtime/demo-store --out .runtime/recovery.json
```

Windows 将 `scripts/run.sh` 替换为 `edgekv.bat`。

交互式 Shell：

```bash
scripts/run.sh shell --data .runtime/demo-store --now 1700000100
```

可用命令：`put`、`get`、`delete`、`expired`、`compact`、`report`、`summary`、`now`、`version`、`help`、`quit`。

## 故障注入

故障点在持久化阶段完成后触发，并由命令行参数选择：

```bash
scripts/run.sh compact --data .runtime/fault --now 1700000100 \
  --fault-at after-snapshot-replace
```

可选值：

| 参数 | 触发位置 | 重启后可见状态 |
| --- | --- | --- |
| `after-log-append` | 日志帧写入并 `fsync` 后 | 新记录已进入日志 |
| `after-snapshot-write` | 新快照原子发布后、清单切换前 | 旧快照 + 完整旧日志 |
| `after-snapshot-replace` | 新清单原子发布后 | 新快照 + 新空日志 |
| `after-log-truncate` | 旧日志轮换到归档后 | 新快照 + 新空日志 |

进程使用 `Runtime.halt(137)` 模拟强制终止，不依赖延迟或修改源码。

## 修复模式

启动检测到快照/日志身份或版本冲突、完整帧 CRC 损坏时，普通命令以退出码 `3` 失败，并向 `stderr` 输出：

```json
{"status":"REPAIR_REQUIRED","code":"E_LOG_TRUNCATED","message":"...","backup":"repair/backup-001"}
```

错误码：

- `E_SNAPSHOT_VERSION_CONFLICT`：清单、快照或日志代际/版本/校验不一致。
- `E_LOG_TRUNCATED`：日志完整帧校验失败或头部不可读，不能按普通尾部残余安全跳过。

执行显式修复：

```bash
scripts/run.sh repair --data .runtime/demo-store
```

修复前会完整复制清单、快照和日志。日志损坏时只截断到最后完整帧；快照损坏时优先选择上一代有效“快照 + 归档日志”，若没有可用快照则从版本从 `1` 开始且连续的完整基础日志重建新日志代。不会静默清空全部历史。

## 确定性演示

Linux/macOS：

```bash
scripts/demo.sh
```

Windows：

```bat
scripts\demo.bat
```

演示会自动执行 8 个场景：固定样本恢复、安全尾部截断、四类故障终止、`E_LOG_TRUNCATED` 修复、`E_SNAPSHOT_VERSION_CONFLICT` 修复。每个场景输出单行 JSON，最后一行固定为：

```json
{"demo":"PASS","scenarios":8,"faultPoints":4}
```

脚本每次先重置 `.runtime/demo`，不读取系统时间，因此连续运行输出逐字节一致。

## 实际验证结果

本工作区执行结果：

```text
Tests run: 13, Failures: 0, Errors: 0, Skipped: 0
demo first run:  PASS
demo second run: PASS
diff(first, second): no differences
```

关键观测：

- 固定样本重启恢复：`replayedRecords=5`、`finalVersion=5`、`live=2`、`expired=1`、`deleted=1`。
- 安全截断：跳过 `50` 字节残余，保留前 4 条完整记录，随后新写入版本从 `5` 继续。
- `after-log-append` 故障：新写入已持久化，恢复后 `finalVersion=6`。
- `after-snapshot-write` 故障：仍为第 1 代、无快照、重放 5 条日志、版本 5。
- `after-snapshot-replace` / `after-log-truncate` 故障：第 2 代快照生效、活动日志记录数 0、版本 5，无回退。
- 不完整帧 CRC 损坏：输出 `E_LOG_TRUNCATED`，备份到 `repair/backup-001`，显式修复后保留 4 条完整记录。
- 快照校验失败：输出 `E_SNAPSHOT_VERSION_CONFLICT`，显式修复后从完整基础日志重建，最终版本仍为 5。

更详细的二进制格式和崩溃一致性协议见 `docs/storage-format.md`。
