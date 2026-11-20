# 图集二进制格式

所有整数和浮点字段为小端。头部固定 48 字节，随后紧跟
`width * height * 4 * 2` 字节的 RGBA16F 通道数据。

## 头部布局

| 偏移 | 类型 | 字段 | 说明 |
| ---: | --- | --- | --- |
| 0 | `8s` | `magic` | 固定 `ATLSHDR1` |
| 8 | `uint16` | `format_version` | 当前为 `1` |
| 10 | `uint16` | `header_size` | 固定 `48` |
| 12 | `uint32` | `width` | 像素宽度 |
| 16 | `uint32` | `height` | 像素高度 |
| 20 | `uint8` | `bit_depth` | `8/10/12/16`，写入通道的量化目标位深 |
| 21 | `uint8` | `color_space_code` | `0=Rec.709`, `1=Display-P3`, `2=Rec.2020` |
| 22 | `uint8` | `transfer_function_code` | `0=linear`, `1=sRGB`, `2=PQ` |
| 23 | `uint8` | `storage_format` | `1=RGBA16F` |
| 24 | `uint16` | `grid_cols` | 区块列数 |
| 26 | `uint16` | `grid_rows` | 区块行数 |
| 28 | `float32` | `source_peak_nits` | 源峰值亮度，1..10000 |
| 32 | `uint32` | `pipeline_version` | 发布版本号；源样本为 0 |
| 36 | `uint32` | `payload_size` | 必须等于宽×高×4×2 |
| 40 | `uint32` | `payload_crc32` | 通道载荷 CRC32 |
| 44 | `uint32` | `header_crc32` | 头部 CRC32，计算时本字段置 0 |

`struct` 格式为：

```python
struct.Struct("<8sHHIIBBBBHHfIIII")
```

## 通道与线性化

- 通道顺序为 `R,G,B,A`，每个通道是 IEEE 754 binary16。
- 图集的 `bit_depth` 是量化目标；RGB 值在写入半浮点前量化到
  `0..(2**bit_depth - 1)`，再映射回 `[0,1]` 存为 float16。
- Alpha 按原始半浮点保存，不参与色彩矩阵。
- 读取后 RGB 按头部传递函数解码：
  - `linear`：恒等；
  - `sRGB`：标准分段函数，正值区间可延伸到 >1 的 HDR 编码；
  - `PQ`：ST 2084 解码为绝对亮度比例，再按 `source_peak_nits` 归一化。
- 色彩运算使用 float64，写出前转 float16。非有限通道值会导致
  `E_ATLAS_INVALID`，字段为 `pixels`。

## 分区索引

网格必须同时整除宽和高。区块边界使用整数比例切分：

```text
x0 = col * width / grid_cols
x1 = (col + 1) * width / grid_cols
y0 = row * height / grid_rows
y1 = (row + 1) * height / grid_rows
```

区块索引按行优先递增，参数表、误差摘要和导出报告都使用同一索引。

## 诊断码

格式层只返回 `E_ATLAS_INVALID`，并带定位字段：

- `magic`
- `format_version`
- `header_size`
- `header_crc32`
- `width` / `height`
- `bit_depth`
- `color_space_code`
- `transfer_function_code`
- `storage_format`
- `grid_cols` / `grid_rows`
- `source_peak_nits`
- `payload_size`
- `payload_crc32`
- `pixels`
