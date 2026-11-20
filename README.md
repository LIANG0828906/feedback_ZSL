# docagg：带精度账本的本地文档聚合执行器

本仓库实现一个不依赖外部服务的列式文档聚合层。输入为 JSON Lines，执行
`sum`、`avg`、`min`、`max`、`stdDev`，支持多字段分组、内存阈值溢出、
确定性分区合并、误差界传播、参考真值验收、声明式舍入和结构化业务错误。

核心聚合逻辑只使用 Python 标准库；NumPy 用于满足本地运行环境要求，并由演示脚本
在启动时检查可导入。

## 快速开始

```bash
uv venv --python /usr/bin/python3 .venv
uv pip install --python .venv/bin/python -r requirements.txt

.venv/bin/python -m unittest discover -s tests -v
./scripts/run_demo.sh
```

`run_demo.sh` 会将固定样本执行两次，逐字节比较主要报告，并输出确定性的
`artifacts/demo/summary.json`。演示内容包括：

- 正常样本的溢出执行、参考真值和误差验收；
- 同一管道的无溢出执行与溢出执行对照；
- 缺字段、不可比较类型、标度冲突、单样本 `stdDev`、缺失分组键；
- 注入第一次溢出写失败后，错误码和已完成分组结果保留。

## 命令行

```bash
.venv/bin/python -m docagg run \
  --input samples/events.jsonl \
  --pipeline samples/pipeline.json \
  --reference \
  --reference-output artifacts/demo/reference.json \
  --output artifacts/demo/normal_report.json \
  --error-output artifacts/demo/normal_errors.json \
  --pretty
```

常用参数：

| 参数 | 说明 |
|---|---|
| `--group-by tenant,region` | 分组字段，可重复 |
| `--sum amount --avg amount` | 快捷指定算子 |
| `--aggregate label=sum:amount` | 指定结果名称、算子和字段 |
| `--rounding sum=half_even@10` | 按算子或结果名称配置舍入模式与有效位数 |
| `--batch-size 4` | 列式批次大小 |
| `--max-active-groups 2` | 活跃分组阈值 |
| `--max-state-bytes 4096` | 累加状态字节阈值 |
| `--spill-partitions 3` | 每次溢出使用的确定分区数 |
| `--fail-spill-on-attempt 1` | 注入第 N 次溢出写失败 |
| `--reference` | 从同一输入生成参考真值并验收 |
| `--reference-file ref.json` | 使用已有参考真值验收 |
| `--error-output errors.json` | 导出错误数组；错误同时逐行写 stderr |

退出码：`0` 默认成功；`2` 出现 `E_PRECISION_BOUND`；`3` 在 `--strict`
下存在其他业务错误；`4` 配置或输入无法读取。默认情况下，可恢复的分组业务错误
不会阻止其他分组输出。

## 管道配置

`samples/pipeline.json` 展示了完整格式：

```json
{
  "group_by": ["tenant", "region", "bucket"],
  "aggregates": [
    {"name": "amount_sum", "op": "sum", "field": "amount"},
    {"name": "amount_stddev", "op": "stdDev", "field": "amount"}
  ],
  "rounding": {
    "sum": {"mode": "half_even", "significant_digits": 10},
    "stdDev": {"mode": "up", "significant_digits": 8}
  },
  "engine": {
    "batch_size": 4,
    "max_active_groups": 2,
    "max_state_bytes": 4096,
    "spill_partitions": 3
  }
}
```

舍入模式：

- `half_even`：半偶舍入；
- `toward_zero`：朝零舍入；
- `up`：远离零舍入；
- `ceiling`：朝正无穷舍入（扩展模式）。

舍入只作用于最终结果，发生在误差估计之后。输出同时保留
`value_before_round`、`rounded_value`、对应的文本形式，并标记
`rounding_stage: after_error_estimation`。`avg` 和 `stdDev` 的中间状态不做
有效位数舍入。`post_round_error_estimate` 将最终舍入步长的保守上界加到舍入前
误差界，便于调用方比较舍入后的值。

## 数值与精度设计

### 来源类型

`ParsedNumber` 为每个输入值保留：

- `source_type`：`float`、`decimal` 或 `integer`；
- 原始值、精确有理数和精确十进制定点值；
- 定点标度、有效数字位数；
- 转换到 binary64 后的绝对表示误差；
- `downgraded` 与精度路径，例如
  `integer->binary64`、`integer-precision-loss`。

超过 binary64 精确表示范围的整数不会被静默当作精确双精度值：执行结果仍可使用
binary64 浮点算子，但误差账本会记录转换损失，结果带 `type_downgraded` 和
`downgrade_count`。完全超出有限双精度范围的值返回 `E_NUMERIC_OVERFLOW`。

同一分组、同一字段的定点小数字符串必须标度一致。整数与定点值可以混合；整数不参与
定点标度集合。若两个定点来源的标度不同，该分组聚合标记为错误，其他分组继续执行。
跨溢出分区才会相遇的不同标度也会在合并阶段重新检查。

### 双通道累加

每个聚合状态维护两条通道：

1. **binary64 执行通道**：`sum`/`avg` 在每批内按绝对值升序，使用 Neumaier
   补偿求和；`stdDev` 使用稳定的 Welford 均值和二阶矩；`min`/`max` 使用
   binary64 比较。
2. **100 位十进制审计通道**：保存高精度和、平方和、绝对值和与极值，用于生成
   误差估计。浮点值通过 `Decimal.from_float` 转为无损十进制；定点与整数保留其
   精确十进制形式。

最终误差估值取以下量的上界加安全余量：

- 来源转换误差；
- 补偿累加的保守舍入界；
- 跨部分聚合合并新增的舍入界；
- 执行结果与高精度审计结果的差值；
- 100 位十进制审计自身的微小舍入余量。

输出中的 `error_components` 分别给出 `conversion`、
`accumulation_and_merge`、`merge_bound`、`audit_gap` 和
`safety_padding`；`error_path` 记录来源、binary64 降级、排序、补偿求和、
Welford、溢出合并和审计阶段。

### 溢出与合并

达到活跃分组数或状态字节阈值时，当前所有分组状态被快照并确定性地轮转分配到
`spill_partitions` 个 JSONL 分区文件。每个事件先写 `.tmp` 文件并 `fsync`，
全部写成功后再原子改名。任何失败都会清理本次临时文件、保留原内存状态，发出
`E_SPILL_WRITE_FAILED`，并关闭后续自动溢出以避免重复故障。

最终按 `spill-序号-part-序号` 字典序合并。同组部分状态可出现在多个溢出事件和文件
中；合并会：

- 汇总 `count`、误差分量和来源精度元数据；
- 对 `sum` 传递补偿项并增加本次合并的舍入界；
- 用 Chan 平行方差合并公式合并 `stdDev` 的 `mean`/`M2`；
- 重新校验跨分区定点标度；
- 记录合并文件顺序、合并前部分状态出现次数、去重前后分组数和一致性布尔值。

`spill.group_count_consistent` 用于核对“部分状态中的唯一分组数”与
“合并后分组数”。声明误差不会在合并后被减小。

### 参考真值

`ReferenceEngine` 使用同一套解析和错误规则：

- 不含浮点来源的分组：`sum`、`avg`、`min`、`max` 使用精确 `Fraction`；
  `stdDev` 对精确样本方差做高精度平方根；
- 含浮点来源的分组：使用 100 位 `Decimal` 定点模拟，浮点值以
  `Decimal.from_float` 无损进入模拟；
- `stdDev` 定义为样本标准差，分母为 `n - 1`。

验收比较的是舍入前结果。绝对偏差超过对应 `error_estimate` 时追加
`E_PRECISION_BOUND`，其 JSON 包含分组键、字段、聚合名、偏差和声明界，并输出到
stderr。

## 输出结构

每个分组包含：

```json
{
  "key": {"tenant": "acme", "region": "north", "bucket": 7},
  "document_count": 8,
  "aggregates": {
    "amount_sum": {
      "status": "ok",
      "value": 9007199254741010.0,
      "value_before_round": 9007199254741010.0,
      "rounded_value": 9007199255000000.0,
      "rounded_value_text": "9007199255000000",
      "error_estimate": 17.67863562364424,
      "post_round_error_estimate": 500017.67863562366,
      "error_estimate_basis": "value_before_round",
      "type_downgraded": true,
      "source_types": {"decimal": 3, "float": 4, "integer": 1},
      "decimal_scales": [4],
      "decimal_precision_digits": [4, 5, 6],
      "error_components": {},
      "error_path": [],
      "precision": {
        "source_types": {"decimal": 3, "float": 4, "integer": 1},
        "decimal_scales": [4],
        "decimal_precision_digits": [4, 5, 6],
        "type_downgraded": true
      },
      "rounding": {},
      "reference_check": {}
    }
  }
}
```

失败聚合保留在组内，结构为：

```json
{
  "status": "error",
  "name": "amount_stddev",
  "op": "stdDev",
  "field": "amount",
  "error_codes": ["E_STDDEV_INSUFFICIENT_SAMPLES"]
}
```

业务错误同时写入顶层 `errors`、`--error-output` 文件和 stderr JSON Lines。每条
错误包含 `code`、`reason`、`group_key`、可选 `field`/`aggregate`、`recoverable`
和 `details`。

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_FIELD_MISSING` | 分组键或数值字段缺失/null |
| `E_TYPE_INCOMPARABLE` | 布尔、非数值字符串或对象进入数值字段 |
| `E_SCALE_MISMATCH` | 同组定点值标度不一致，含跨分区合并发现 |
| `E_STDDEV_INSUFFICIENT_SAMPLES` | `stdDev` 样本数为 0 或 1 |
| `E_SPILL_WRITE_FAILED` | 溢出临时/分区文件写入失败，已保留内存状态 |
| `E_PRECISION_BOUND` | 与参考真值比较后超出声明误差 |
| `E_NUMERIC_OVERFLOW` | 输入或聚合超出有限双精度范围 |
| `E_GROUP_KEY_INVALID` | 分组键不是可序列化标量 |
| `E_PARTIAL_STATE_INVALID` | 溢出部分状态损坏或无法反序列化 |
| `E_INVALID_CONFIG` | 管道、舍入或内存配置非法 |
| `E_INPUT_INVALID` | 输入文件或 JSONL 无法读取 |

## 实际验证结果

本次在 Python `3.14.4`、NumPy `2.5.5` 下执行：

```text
22 tests passed
正常样本：40 条文档，5 个分组，10 次成功溢出，30 个分区文件
合并前部分状态出现 40 次，去重前 5 个分组，合并后 5 个分组
正常样本 precision failures = 0，所有参考检查均在声明误差内
无溢出对照：5 个分组，成功溢出 0 次，所有参考检查通过；溢出执行误差界不小于无溢出执行
异常样本：8 个分组，26 条结构化错误
错误码：E_FIELD_MISSING / E_TYPE_INCOMPARABLE / E_SCALE_MISMATCH /
        E_STDDEV_INSUFFICIENT_SAMPLES
溢出注入：E_SPILL_WRITE_FAILED，失败 1 次，仍保留 5 个分组结果
确定性：两次运行 5 个主要报告逐字节一致
```

固定样本哈希：

- `samples/events.jsonl`：
  `4f02e1a5490e178985a603cb64e50cd076d5dedbbd9e57bb780fcd940069b94d`
- `samples/anomalies.jsonl`：
  `3dacbc9a5ea209fc9d718395b360469103ffa6f2cdcd08a993339e4ebcb768b3`
- 正常报告确定性摘要：
  `017c057a02d2a118de0f5972cb260f62f8b70b9f2d5c75a4dce6c50e4deb18d9`

完整机器可读记录见 `artifacts/demo/summary.json`，聚合结果和参考真值分别见
`artifacts/demo/normal_report.json` 与 `artifacts/demo/reference.json`。
