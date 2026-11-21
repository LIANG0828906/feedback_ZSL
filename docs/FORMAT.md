# 持久化格式

所有整数和浮点字段均按小端固定宽度编码。文本 JSON 只用于检查和报告，恢复判定以二进制文件和 SHA-256 为准。

## 1. 输入/输出网格 `.mesh`

固定 48 字节头部：

| 偏移 | 大小 | 字段 |
|---:|---:|---|
| 0 | 8 | magic `MRMESH01` |
| 8 | 4 | version = 1 |
| 12 | 2 | coordinate type: 0=`float32`, 1=`float64` |
| 14 | 2 | index width: 4 或 8 |
| 16 | 8 | vertex count |
| 24 | 8 | face count |
| 32 | 4 | flags，当前必须为 0 |
| 36 | 4 | header size = 48 |
| 40 | 8 | payload byte length |

Payload 依次为：

1. `vertex_count` 个 `(x,y,z)` 坐标，分量宽度由 coordinate type 决定。
2. `face_count` 个三索引面，每索引宽度由 index width 决定。

校验顺序：

- magic/version/header size；
- coordinate type 与 index width 的合法值；
- 头部声明的 payload 长度和实际文件长度必须同时等于 `vertex_count*3*coord_size + face_count*3*index_width`；
- 每个坐标必须有限；
- 每个索引必须小于 `vertex_count`。

索引越界返回 `E_MESH_INVALID` 并填写第一个错误 `face`；非有限坐标填写第一个错误 `vertex`；面片区长度不足时填写首个缺失/异常面片索引。

`repaired.mesh` 始终使用 `float64 + uint64` 输出，顶点按稳定 ID 排序，面片按分块 ID 和分块内规范顺序输出。

## 2. `plan.bin`

单个原子提交文件，包含分块表与边界映射两个带版本 section：

```text
magic "MRPLAN01"
u32 format version = 1
string input_hash
string config_hash
double chunk_size_x / y / z
u32 table_version = 1
string table_hash
u64 table_payload_size
bytes table_payload
u32 boundary_version = 1
string boundary_hash
u64 boundary_payload_size
bytes boundary_payload
sha256(body)[32]
```

`table_payload` 保存输入哈希、分块尺寸、所有分块的 ID/单元/预估内存/有序面片/有序顶点，以及 straddle 面片的 touched cell 清单。`table_hash` 只依赖输入和分块尺寸，不依赖并行度或内存参数。

`boundary_payload` 保存 table hash 和每个相邻块 pair 的有序顶点 ID、有序边端点。`boundary_hash` 是规范 payload 的 SHA-256。

`plan_hash` 用于块链起点，是 body（不含末尾 32 字节 checksum）的 SHA-256。

## 3. `chunk_XXXXXX.mrchunk`

每块一个文件，一次临时文件写入、`fsync`、原子重命名提交：

```text
magic "MRCHUNK1"
u32 format version = 1
u32 chunk id
string input_hash
string table_hash
string boundary_hash
string local_boundary_hash
string prev_chain_hash
u64 filled_faces
u64 split_groups
u64 vertex_count
  { u64 id, double x, double y, double z } ...
u64 face_count
  { u64 source_face, u64 v0, u64 v1, u64 v2,
    u32 owner_chunk, u8 is_fill, string hole_id } ...
sha256(body)[32]
```

`prev_chain_hash` 对块 0 等于 `plan_hash`，对其他块等于前一个已提交 bundle 的 body 哈希。提交只按块 ID 顺序进行。解析后必须同时满足：

- bundle 内容 checksum 正确；
- chunk ID、input/table/boundary/local boundary 哈希全部匹配；
- 链上前驱哈希匹配；
- 不存在“后续块已提交、前序块缺失”的空洞。

任何一项失败均返回 `E_RECOVERY_INVALID`，`chunk` 字段指向首个断链/损坏块。

## 4. 文本哈希

- `table_hash`：规范 chunk table payload 的 SHA-256。
- `boundary_hash`：规范 boundary payload 的 SHA-256。
- bundle hash：bundle body 的 SHA-256，不含末尾 checksum 的 32 字节。
- `stitch_hash`：最终 `repaired.mesh` 完整文件字节的 SHA-256。
- `stitch.sha256`：`<stitch_hash>  repaired.mesh\n`。
