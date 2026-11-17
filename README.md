# terrain_bake — 开放世界地形分块烘焙组件

确定性的地形分块烘焙流水线：按固定网格切分高度场地形，逐块输出顶点位置、
法线、UV 与 Lambert 烘焙纹理，消除块间法线接缝，并支持被强制终止后仅凭
已提交快照断点续焙，结果与一次性完整烘焙逐字节一致。

## 环境

- Python 3.9+，依赖 `numpy`、`pillow`（测试另需 `pytest`）
- 全部样本由任务代码确定性生成，不访问网络、不依赖外部图片/模型

## 使用

```bash
# 烘焙（可重复执行；存在有效快照时自动续焙）
python -m terrain_bake bake --out out/run --seed 20261118 \
    --chunk-verts 33 --chunks-x 8 --chunks-y 8

# 逐字节对比两个烘焙产物目录（不一致时输出结构化报告并返回非零）
python -m terrain_bake verify out/oneshot out/resumed

# 测试钩子：提交 N 块后模拟 SIGKILL 硬退出
python -m terrain_bake bake --out out/run --crash-after 4
```

## 产物结构

```
out/run/
  chunks.json            # 块清单：网格推导、每块坐标/顶点数/包围盒/四邻接/输出哈希
  report.json            # 烘焙报告（字段顺序稳定，无时间戳与绝对路径）
  .bake_checkpoint.json  # 进度快照（原子提交，完成后 status=complete）
  chunks/chunk_<cx>_<cy>/
    positions.npy        # (n, n, 3) float64 世界坐标
    normals.npy          # (n, n, 3) float64 单位法线（接缝已修复）
    uv.npy               # (n, n, 2) float64，覆盖 [0,1]，与块索引无关
    texture.png          # n x n RGBA，Lambert + gamma(1/2.2)
```

## 设计要点

- **确定性高度场**：自实现 SplitMix64 整数散列生成格点值，多倍频值噪声
  （smoothstep 插值），同一参数多次生成逐字节一致。
- **分块**：块边长 `n` 个顶点、步进 `n-1`，相邻块共享边界顶点，既不重叠
  也不遗漏；全局顶点数 `chunks * (n-1) + 1`。
- **法线与接缝修复**：顶点法线由高度场中心差分得到（世界边界退化为前/后
  向差分）。每块先做"仅本块"的独立计算，再对边界环采样相邻块高度（halo）
  重算并替换，使共享边界顶点法线逐分量完全一致，且与全局一次性计算在
  1e-6 容差内一致；被修复的边界顶点数计入快照与报告。
- **可恢复流程**：每块产物落盘并记录 SHA-256 后原子更新快照（tmp+rename）。
  重启时校验快照版本、输入摘要与已提交文件哈希；快照损坏、摘要不一致或
  文件被篡改时整体丢弃、从头烘焙，绝不沿用旧块偏移。
- **报告**：`report.json` 含种子、输入/高度场摘要、总块数、总顶点数、修复
  边界顶点数、各块纹理 SHA-256、恢复次数；唯一不确定字段为 `elapsed_ms`。

## 验证结果（本机实测）

- `pytest tests/`：10 项全部通过（高度场确定性、分块覆盖、单位法线、
  接缝修复与全局一致性、纹理边界无跳变、崩溃恢复逐字节一致、损坏/过期
  快照丢弃、报告确定性、校验器结构化报错、清单内容）。
- 验收场景（seed=20261118，8x8 块、每块 33 顶点，全局 257x257）：
  - 一次性烘焙：64 块、66049 顶点、修复边界顶点 7196 个；
  - 另一进程烘焙中被真实 `kill -9`（快照已提交 22 块），重启仅凭快照续焙，
    `resume_count=1`；
  - `verify` 对比两者全部 64 块的 positions/normals/uv/texture：
    `{"ok": true, "mismatches": []}`，退出码 0；
  - 两次报告除 `elapsed_ms` 与 `resume_count` 外完全一致。

## 测试

```bash
python -m pytest tests/ -q
```
