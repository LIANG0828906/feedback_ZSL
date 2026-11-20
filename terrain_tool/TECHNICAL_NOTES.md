# 地形可视化增量更新工具 — 技术说明

## 1. 总体架构

纯 Python 实现，离屏渲染只依赖 NumPy 与 PySide6 的 `QImage`，不使用 GPU。

| 模块 | 职责 |
| --- | --- |
| `binary_io.py` | 自定义二进制头 + 原始体数据的读写与严格校验 |
| `hashing.py` | 内容哈希（SHA-256）与哈希链 |
| `quadtree.py` | 规则分块、粗糙度/LOD 层级、四叉树构建 |
| `pipeline.py` | 中心差分法线、材质双线性混合、接缝闭合/检测 |
| `dependency.py` | 链路节点建模与增量失效闭包 |
| `engine.py` | 加载、补丁校验合并、增量/全量两条计算路径 |
| `snapshot.py` | 临时文件 + 原子重命名的批次提交与恢复 |
| `render.py` | NumPy 栅格化、瓦片缓存、增量重渲、QImage 导出 |
| `worker.py` | QThread 后台任务、E_BUSY、协作式取消 |
| `samples.py` | 固定种子的确定性地形/补丁/基线哈希生成 |
| `ui.py` / `cli.py` | PySide6 界面与命令行入口 |

## 2. 二进制格式

所有头部小端、`struct` 无填充。

- `.hgt`（44 字节头）：magic、宽、高、格距、高程类型(=float32)、flags、
  有效矩形 x/y/w/h、基线版本、保留字；体数据为 float32。
- `.mat`（32 字节头）：magic、宽、高、索引类型(uint8/uint16)、flags、
  基线版本、3 个保留字。
- `.ptc`（44 字节头）：magic、补丁类型、数据类型、目标矩形 x/y/w/h、
  序号、格距、基线版本、2 个保留字。

长度不符、magic/类型错误、有效矩形越界、非有限高程等一律返回
`E_TERRAIN_INVALID` / `E_PATCH_INVALID`，且不建立任何节点。

## 3. 增量失效判定

链路：`HEIGHT→NORMALS→RENDER`、`HEIGHT→LOD→SEAM→RENDER`、
`MATERIAL→BLEND→RENDER`。

- 补丁矩形先映射到直接受影响 chunk；
- 法线与材质混合依赖相邻一圈像素，失效带 1 chunk 光环；
- LOD 变化让相邻块接缝失效，接缝同样带 1 chunk 光环；
- 迭代到不动点得到完整闭包，每个失效节点都记录原因；
- 复用块必须通过「高程哈希 + 材质哈希 + LOD 层级」三项证据校验，
  无法证明未受影响时保守重算，绝不静默复用陈旧节点。

## 4. 一致性保证（增量 == 全量，逐字节）

两条路径调用同一批纯函数。为保证瓦片缓存可安全复用，渲染刻意只依赖
块局部输入：

- 高程热力使用引擎初始化时固定的 `display_range`；
- 误差热力使用初始化时固定的 `heat_scale`；
- 法线/混合函数对相同字节输入产出相同字节输出。

因此增量重渲仅覆盖失效块像素，最终颜色/热力缓冲与同参数全量渲染
**逐字节相等**，且固定参数下连续两次渲染哈希一致。

## 5. 接缝处理

四叉树相邻叶子深度不同即存在 LOD 边界。粗边两个端点高程之间按世界距离
线性插值，与细边每个顶点的真实高程比较，最大垂直偏差超过
`crack_tolerance` 时返回 `E_CRACK_UNRESOLVED` 并给出接缝世界坐标；
否则记录裙边/匹配顶点修正量并标记 `closed`。

## 6. 原子提交与恢复

每批先写 `*.tmp`、`flush + fsync`，再 `os.replace` 原子替换数据文件与
批次清单，最后替换 `manifest.json` 指针。指针存在即代表该批完整可见。

- 任意步骤失败：回滚本批已替换文件，上一版本保持不变，返回 `E_COMMIT_FAILED`；
- 恢复：清理 `*.tmp` 残留，从 seq=1 顺序重放批次清单，校验 `prev_chain`
  哈希链，断链/缺字段返回 `E_RECOVERY_INVALID`；
- 恢复后可从最近批次之后继续应用新补丁。

## 7. 后台任务与取消

`RecomputeManager` 内部用 `QThread`，同一引擎同时只允许一个任务，
重复 `submit` 抛 `E_BUSY`。任务通过 `CancellationToken` 在检查点协作式
中断；取消后释放离屏瓦片缓冲，磁盘最后一次已提交快照不受影响。

## 8. 错误码对照

| 错误码 | 触发场景 |
| --- | --- |
| `E_TERRAIN_INVALID` | 高度场/材质头矛盾、长度不符、非有限高程 |
| `E_PATCH_BOUNDS` | 补丁矩形越出有效范围 |
| `E_PATCH_STALE` | 补丁基线版本与当前版本不一致 |
| `E_PATCH_INVALID` | 补丁格式/格距/数据非法或含非有限高程 |
| `E_CRACK_UNRESOLVED` | 接缝垂直偏差超容差 |
| `E_COMMIT_FAILED` | 批次提交写入失败，上一版本保留 |
| `E_RECOVERY_INVALID` | 快照哈希链断裂或字段缺失 |
| `E_BUSY` | 已有重算任务运行中重复启动 |
| `E_CANCELLED` | 重算任务被协作式取消 |
