# 分块存档格式

所有整数和浮点数均为小端序。格式不使用隐式默认值；可选字段是否存在由显式
位图记录。解析失败返回 `E_CHUNK_INVALID`，位置包含分块号和字段名。

## 文件头（32 字节）

| 偏移 | 类型 | 字段 |
| ---: | --- | --- |
| 0 | `u8[8]` | 魔数 `CHUNKARC1` |
| 8 | `u16` | 版本，允许 `1..3` |
| 10 | `u16` | 文件头长度，固定 `32` |
| 12 | `u32` | 场景 ID |
| 16 | `f32` | 边界可见比例容差，范围 `[0,1]` |
| 20 | `u32` | 分块数 |
| 24 | `u32` | 保留，必须为 `0` |
| 28 | `u8[4]` | 保留 |

## 分块公共前缀

| 类型 | 字段 |
| --- | --- |
| `u32` | 分块 ID，全文件唯一 |
| `f32 x6` | AABB：`min.xyz`, `max.xyz` |
| `u32` | 顶点数 |
| `u32` | 索引数 |

顶点格式为 `f32 x3`，索引为 `u32`。索引数必须是 3 的倍数，索引必须小于顶点数。
所有顶点必须落在 AABB 内；否则报字段 `bounds`。

## v1 负载

```text
vertices[vertex_count]
indices[index_count]
```

v1 没有网格划分、遮挡体标记、压缩描述、优先级和层级表。载入后：

- 上述字段保持“不存在”，不会静默补成 `false` 或空表。
- 优先级按公开规则推导，来源标为 `derived-from-bounds`。

## v2 负载

```text
vertices[vertex_count]
indices[index_count]
u16 field_flags
[grid if bit 0]
[occluder if bit 1]
```

- bit 0：存在 `u32 grid_x`, `u32 grid_y`, `u32 grid_z`, `f32 cell_size`。
- bit 1：存在 `u8 occluder`，必须为 `0` 或 `1`。
- 其他位非法。

v2 没有压缩、优先级和层级表；优先级同样按公开规则推导并标注来源。

## v3 负载

```text
u16 field_flags
[compression + payload if bit 2, otherwise raw mesh]
[grid if bit 0]
[occluder if bit 1]
u32 priority
u32 priority_source
[hierarchy if bit 3]
```

- bit 0：网格划分。
- bit 1：遮挡体标记。
- bit 2：压缩描述及压缩负载。
- bit 3：层级引用表。
- bit 4：优先级字段存在；v3 中该位必须为 1，否则字段 `priority` 无效。
- 其他位非法。

压缩描述：

| 类型 | 字段 |
| --- | --- |
| `u32` | codec：`0` 原始字节，`1` RLE |
| `u32` | 压缩参数标志 |
| `u64` | 原始网格字节数 |
| `u64` | 存储字节数 |
| `u64` | 后续负载字节数，必须等于存储字节数 |
| `u8[]` | 负载 |

原始网格字节数必须等于 `vertex_count * 12 + index_count * 4`。
RLE 负载由若干 `u8 value + u32 count` 组成，解码后长度必须等于声明值。
未知 codec 报 `compression.codec`。

优先级来源：

- `0`：v3 原始声明，记为 `authored-v3`。
- `1`：旧版升级时按 AABB 推导，记为 `derived-from-bounds`。

层级表：

```text
u32 parent_chunk_id   // 0xffffffff 表示无父节点
u32 child_count
u32 children[child_count]
```

父引用、子引用必须存在且非悬空；禁止自引用、重复子节点和环。父节点必须存在
层级表并反向列出该子节点。

### 显式缺席

v3 位图允许显式表示“旧版升级后仍不存在”的网格、遮挡体、压缩和层级属性，
用于无损保留来源语义。位为 0 表示该字段被明确声明为不存在，不是解析默认值。
字段在位为 1 后截断或长度错误仍会报 `E_CHUNK_INVALID`。

## 优先级推导规则

令 `d = |center(AABB)|`，其中 AABB 中心相对世界原点：

```text
q = floor(d * 16)
priority = clamp(999999 - q, 0, 999999)
priority_source = derived-from-bounds
```

队列按 `priority` 降序、视点到 AABB 中心距离升序、分块 ID 升序排序。

## 版本降级损失

`E_VERSION_LOSS` 在写入前整体检查，不产生部分文件。

- 目标 v2：不能表达 v3 压缩描述、v3 原始优先级、层级表。
- 目标 v1：除上述字段外，还不能表达网格划分和遮挡体标记。
- 由旧版推导出的优先级不视为 v3 原始声明，因此旧版导出到 v1/v2 不产生该损失。

每个损失位置包含 `chunk_id` 和字段名。工程不会静默丢弃遮挡体标记或层级引用。

