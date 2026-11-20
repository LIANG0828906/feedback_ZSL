# 本地分片对象存储网关

仅使用 Python 3.11+ 标准库实现。网关把固定大小分片并行写入本地磁盘，所有分片通过摘要与长度校验后再原子发布对象清单。进程中断后可以从已确认分片继续；未确认的临时文件、孤儿版本文件和残缺尾部不会被计入提交状态。

## 快速开始

```bash
python3 --version
python3 -m unittest discover -v
./demo.sh
```

演示脚本只读取本地确定性样本，不访问网络、不读取系统当前时间。它会重建 `demo-work/`，生成固定样本、执行上传并输出规范化 JSON 摘要。

## 目录

```text
shard_gateway/
  storage.py      会话、分片落盘、原子提交、中止、清理
  scheduler.py    在途数量与空间预算受限的并行调度器
  recovery.py     重启扫描、孤儿清理、清单对账、续传区间
  verifier.py     独立一致性校验工具
  cli.py          命令行入口
tests/test_gateway.py
demo.py / demo.sh
```

运行时磁盘布局：

```text
<root>/
  sessions/<session-id>/
    meta.json
    ABORTED
    shards/shard-000001.v000002.bin
    tmp/*.part
  manifests/<sha256(object-key)>.json
```

`meta.json` 和对象清单都通过“同目录临时文件 + `fsync` + `os.replace` + 目录 `fsync`”发布。分片文件使用版本化文件名，元数据只会引用已经整体替换完成的分片文件。

## 接口

Python API：

```python
gateway = UploadGateway("./data")
gateway.create_session(
    session_id="upload-01",
    object_key="bucket/object.bin",
    shard_size=4096,
    total_shards=9,
    expected_size=33545,
    expected_sha256="...",
)
gateway.write_shard(session_id="upload-01", shard_number=1, data=b"...")
gateway.commit_session("upload-01")
gateway.abort_session("upload-01")
```

命令行：

```bash
python3 -m shard_gateway make-sample --path sample.bin --size 33545
python3 -m shard_gateway upload \
  --root ./data --session-id upload-01 --object-key bucket/object.bin \
  --sample sample.bin --shard-size 4096 \
  --max-inflight 3 --space-budget 12288 --session-quota 33545
python3 -m shard_gateway state --root ./data --session-id upload-01
python3 -m shard_gateway recover --root ./data
python3 -m shard_gateway verify --root ./data
python3 -m shard_gateway abort --root ./data --session-id upload-01
python3 -m shard_gateway cleanup --root ./data --session-id upload-01
```

提交错误码：

- `E_SHARD_GAP`：分片编号不连续或有缺失。
- `E_SHARD_DIGEST_MISMATCH`：文件实际摘要、长度、对象整体摘要或调用方期望值不一致。
- `E_SESSION_ABORTED`：会话已中止，写入和提交均被拒绝。
- `E_MANIFEST_INCOMPLETE`：清单结构不完整或无法与分片交叉核对。

中止使用持久化 `ABORTED` 标记，先于元数据状态更新发布，因此即使中止过程中崩溃，提交路径仍会拒绝该会话。

## 并行与空间预算

调度器维护按编号升序排列的等待队列。只有队首满足条件时才会出队，因此不会跳过小编号分片。每次落盘完成后重新填充可用名额。

- `max_inflight`：同时在途分片数上限。
- `--space-budget`：可等待的在途落盘/替换字节预算。超预算的队首进入等待，已有分片确认后释放预留并继续调度。
- `--session-quota`：单会话持久分片文件总字节硬上限；提交前始终满足，无法通过等待释放，因此作为可行性上限而不是等待队列条件。

调度摘要包含 `inflight_peak`、`budget_bytes_peak`、`wait_count` 和完成分片数。若把“持久总量”直接作为等待条件，预算小于对象总大小时永远不可能靠等待完成；因此实现将可等待的在途空间和持久总量配额分开约束。

## 原子提交与故障恢复

提交步骤：

1. 校验 `1..total_shards` 连续、每个分片文件长度和 SHA-256 与元数据一致，并校验对象总长度与对象摘要。
2. 生成临时对象清单，回读临时清单并重新校验。
3. 原子替换发布最终清单并 `fsync` 清单目录。
4. 原子更新会话为 `committed`。

故障注入只能由 `--fault-phase` 参数触发，并使用 `os._exit(86)` 绕过 `finally` 和正常异常退出：

- `shard_landed`：分片文件已整体替换，但元数据尚未更新。
- `metadata_updated`：分片元数据已原子更新，旧版本尚未清理。
- `manifest_published`：最终对象清单已完整发布，会话提交标记尚未更新。

恢复规则：

- 版本化但未被元数据引用的分片文件视为未确认孤儿，删除并计入丢弃字节。
- `tmp/*.part` 中的残缺尾部不计入已完成分片，删除并计入丢弃字节。
- 仅保留实际长度、摘要都与会话元数据一致的已确认分片。
- 已完整发布且可交叉校验的清单会把对应会话恢复为 `committed`。
- 输出 `recovered_shards`、`discarded_bytes`、`continuable_ranges` 和已完成分片摘要。

## 一致性校验

`verify` 不经过上传/提交路径，直接读取会话元数据、分片文件和已发布清单：

- 清单摘要或长度与分片不一致时输出 `E_SHARD_DIGEST_MISMATCH` 到 stderr。
- 清单缺片、编号不完整、会话分片缺失或已提交会话缺清单时输出 `E_MANIFEST_INCOMPLETE` 到 stderr。
- JSON 报告始终写到 stdout，包含对象摘要、清单摘要、会话状态和问题列表。

## 实际验证结果

环境：Python `3.14.4`，Linux 本地文件系统，验收时可用空间约 `923 GiB`。

```text
python3 -m unittest discover -v
Ran 13 tests in 0.817s
OK
```

`./demo.sh` 使用 `33545` 字节固定样本，分片大小 `4096`，共 `9` 个分片。连续运行两次的 JSON 输出逐字节一致：

```text
sample SHA-256:
e8da3c525c849e174b6a846eb6db19595f621860ab6e2c4cdf1f33ccf9d06e95

normal:   inflight_peak=3, wait_count=0, object digest match=true
budget:   max_inflight=8, space_budget=8192, inflight_peak=2,
          wait_count=7, object digest match=true
duplicate: version 10 -> 20 (retry writes) -> 21 (commit),
          shard files after cleanup=9, object digest match=true

fault shard_landed:     exit=86, recovered_version=2 (not lower),
                        recovered_shards=1, discarded_bytes=4096,
                        completed digest match=true
fault metadata_updated: exit=86, recovered_version=3,
                        recovered_shards=2, discarded_bytes=0,
                        completed digest match=true
fault manifest_published: exit=86, manifest visible before recovery=true,
                        recovered status=committed, recovered_version=12

final verify: ok=true, objects=6, all object digest match=true
demo output SHA-256 (two identical runs):
7018af227170106e5f144b9100d8258489b17e70cb878817109f0f867d6222ed
```
