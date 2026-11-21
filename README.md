# StreamChunk：流式分块遮挡剔除与恢复工具

本工程把历史存档的 v1、v2、v3 场景分块统一为内部 v3 表示，并接入同一套
视锥剔除、遮挡体深度金字塔（HZB）查询、确定性离屏可见性输出、不可变版本
发布、强制终止恢复和多版本差异归因管线。

## 已实现能力

- v1/v2/v3 二进制解析，统一内部模型；字段缺失、未知版本、悬空层级引用、
  索引越界、AABB 与顶点不一致均返回 `E_CHUNK_INVALID`，定位分块号与字段名。
- 旧版载入时保留“字段不存在”语义；流式优先级按公开规则推导并标注
  `derived-from-bounds` 来源。降级导出无法表达的属性返回 `E_VERSION_LOSS`
  并逐分块、逐字段列出。
- 按优先级与视点距离调度分块加载，队列去重、并行加载、按分块号确定性发布。
  加载失败记录 `E_LOAD_FAILED`，失败分块不进入可见性掩码。
- 先视锥剔除，再使用离屏深度图和最小深度金字塔做遮挡查询；输出掩码、统计、
  临界分块和边界采样。边界可见比例超差返回 `E_VISIBILITY_MISMATCH`。
- 已加载集合与掩码封装为不可变 `FrameSnapshot`，通过 `shared_ptr` 原子替换；
  旧版本在发布期间持续可读，版本或内容哈希不符返回 `E_VERSION_STALE`。
- 每分块快照使用临时文件写完后原子重命名提交，快照内包含网格、AABB、
  遮挡体标记、优先级、层级、内容哈希和调度状态；恢复校验版本与哈希链，
  跳过已提交分块，断链返回 `E_RECOVERY_INVALID` 并定位首个异常分块。
- 单场景单任务约束，重复提交返回 `E_BUSY`；加载完成和剔除批次检查点均响应
  协作式取消，取消后保留最后一次已发布快照并释放本轮候选缓冲。
- 固定输入下掩码、统计和内容哈希逐字节确定；v1/v2/v3 对比输出差异分块清单，
  并将原因归为 `format_degradation`、`priority_derivation` 或 `load_order`。
- 提供本地确定性样本生成器、基线哈希、JSONL 轨迹日志、差异报告和
  CLI 示例界面。

## 构建

要求 CMake 3.20+、支持 C++17 的编译器；Windows 可使用 Visual Studio 2022。

Windows：

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
ctest --test-dir build -C Release --output-on-failure
powershell -ExecutionPolicy Bypass -File scripts\run_acceptance.ps1
```

Linux/WSL：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j 4
ctest --test-dir build --output-on-failure
scripts/run_acceptance.sh artifacts/acceptance
```

## 快速使用

```text
chunkpipe generate --out artifacts/acceptance/samples
chunkpipe run --archive artifacts/acceptance/samples/scene_v3.arc \
  --out artifacts/acceptance/run-v3 --workers 3
chunkpipe compare --dir artifacts/acceptance/samples \
  --out artifacts/acceptance/compare
chunkpipe compare --dir artifacts/acceptance/samples \
  --out artifacts/acceptance/compare-partial --max-loaded 3
chunkpipe determinism --archive artifacts/acceptance/samples/scene_v3.arc \
  --out artifacts/acceptance/determinism
chunkpipe export --archive artifacts/acceptance/samples/scene_v3.arc \
  --out artifacts/acceptance/scene_v3_to_v2.arc --to-version 2
chunkpipe run --archive artifacts/acceptance/samples/scene_v3.arc \
  --out artifacts/acceptance/crash --crash-after 3
chunkpipe recover --archive artifacts/acceptance/samples/scene_v3.arc \
  --out artifacts/acceptance/crash
```

常用运行参数：`--workers`、`--delay-us`、`--fail 4,7`、
`--cancel-after N`、`--max-loaded N`、`--crash-after N`、`--no-recovery`。

## 输出文件

- `trajectory.jsonl`：调度、加载、恢复提交、剔除、发布和失败轨迹。
- `visibility.mask`：按分块号升序每块一个字节的可见性掩码。
- `visibility.json`：掩码、加载集合、临界分块、边界采样和版本哈希。
- `stats.json`：加载、视锥剔除、遮挡剔除、可见和临界统计。
- `diff_report.json` / `diff.jsonl`：多版本差异及归因字段。
- `determinism.json`：连续两次渲染的逐字节比较结果。
- `error.json`：失败命令的业务码、分块、字段、边界段与细节。
- `recovery/*.snap`：逐分块原子提交的恢复快照。

## 业务码

| 业务码 | 含义 |
| --- | --- |
| `E_CHUNK_INVALID` | 格式、字段、引用或索引无效 |
| `E_VERSION_LOSS` | 目标版本无法表达源属性 |
| `E_LOAD_FAILED` | 分块加载失败 |
| `E_VISIBILITY_MISMATCH` | 相邻分块边界可见性超差 |
| `E_VERSION_STALE` | 发布版本或内容哈希不匹配 |
| `E_RECOVERY_INVALID` | 快照版本、字段或哈希链断链 |
| `E_BUSY` | 同一场景已有流式剔除任务 |
| `E_CANCELLED` | 协作式取消 |

## 文档与验证

- [二进制格式](docs/FORMAT.md)
- [管线设计](docs/DESIGN.md)
- [实际验收记录](docs/ACCEPTANCE.md)

本轮实际验证结果：408 项自动检查通过；v1 掩码
`070707070707070707`，v2/v3 掩码 `1f1f1f030303030303`；v3 内容哈希
`a41580146ff083b8`；强制终止时已提交 3 块，恢复后提交 9 块且最终哈希一致。

## 渲染后端说明

为保证“固定输入逐字节一致”，本交付使用不创建窗口的确定性软件离屏帧缓冲
和最小深度金字塔。渲染、剔除、掩码和统计不依赖 GPU 驱动。若部署环境必须
使用 OpenGL 4.1 帧缓冲，可替换 `renderVisibility` 内部的深度图生成适配层，
发布、恢复、调度和错误语义保持不变。当前验收容器没有 Windows OpenGL 驱动，
因此本轮实测采用软件参考后端；Windows PowerShell 验收入口已提供。
