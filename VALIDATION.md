# Validation Record

本记录对应一次实际本地运行，不是方案描述。验证环境：

```text
OS: Linux/WSL2 x86_64
Python: 3.14.4
NumPy: 2.5.5
GPU/容器/网络: 未使用
```

## Automated Tests

命令：

```bash
python -m unittest discover -s tests -v
```

结果：

```text
Ran 9 tests in 16.571s
OK
```

覆盖项：

- 14 类非法场景输入均返回退出码 `2`，每条错误含 `code/location/field/message`。
- 同一 `texture_seed + resolution` 的星点位置、亮度和量化纹理逐字节一致；换种子后纹理摘要改变。
- 两次独立渲染的 RGBA、层归属图、投影清单和稳定统计逐字节一致；同一目录重复运行统计仍一致。
- 层数组反转、三角面数组反转后 RGBA/层图/投影清单仍逐字节一致。
- 投影落点随方向光改变；越界落点标记 `accepted: false` 且 `shadow_gray: null`。
- 层距增加、光源距离增加时半影宽度和 10%-90% 边缘过渡宽度均增加。
- 真实 `SIGKILL` 中断后续算，三类核心产物与不中断基线逐字节一致。
- `SIGINT` 完成当前块、提交快照、退出码 `0`，后续续算逐字节一致。
- 损坏快照和场景摘要不一致快照被整体丢弃，输出与全新运行一致。
- 伪造“校验自洽但像素被改”的快照后，恢复重算比对输出结构化差异，退出码 `5`。

## Full Sample

场景：`scenes/sample_scene.json`，320×200，25 个 64×40 分块，固定 2×2 子像素，
每块 16 个半影盘样本，投影子纹素密度 2。

首次完整运行：

```text
ELAPSED=1.92s
RSS≈62.7 MB
```

最终基线产物位于 `artifacts/sample/`。文件 SHA-256：

```text
rgba.png         84786c9ec39992573fee993d616e40c329328a218f3b29a7fe55854fcdd88649
layer_map.png    a112e2ec7c1d500ac119a9449cfbac7e022af35d2fe1912ac9ae4d6481300f2d
projections.json c8f3ab9e6f17d4ed600bdb2ce7d7961c42b3ec0f5199061af68283b6b300d165
report.json      84da9330ac3e33d49c9cfcaf0c16065f7b94f1179ce5b0a72e412812e8ca4306
snapshot.json    ca907fc53e7d0b351f66dbd677f9db9d016f1341bca46d0f82e0b9aa5e39bfad
```

像素载荷摘要：

```text
RGBA16 raw digest:       0b560232dfa0eb0e1147bce3c2ae053de0caa15d3eb89ac443dce53c3e53ca07
Layer-map raw digest:    01852d75c92eb343725f38a14d85436e53c6e4489e05d3253b4e9d502b3a9986
Projection report digest:9592700721a5c63b872a6f18dffd089f278b428916b53772b2a2e3781925d080
Projection landing points: 4500 accepted / 4500 total
```

层级统计：

| layer | visible fragments | avg transmission | stars | transmission ratio | reflection ratio |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 0 (inner) | 55,052 | 0.431315046 | 67 | 0.960318193 | 0.039681807 |
| 1 | 49,802 | 0.323339155 | 61 | 0.947190930 | 0.052809070 |
| 2 (outer) | 45,304 | 0.237329798 | 63 | 0.928496926 | 0.071503074 |

半影趋势（同一边缘、同一 `shadow_samples=16`）：

| scenario | gap | source distance | world width | 10%-90% width (px) | gray digest prefix |
| --- | ---: | ---: | ---: | ---: | --- |
| base | 0.45 | 5.5 | 0.075743624 | 2.085226033 | `48880ec8edf00262` |
| gap_x2 | 0.90 | 5.5 | 0.134987248 | 2.696999062 | `2057357782066e34` |
| source_plus_2 | 0.45 | 7.5 | 0.081743624 | 2.153602570 | `d8c81843351ab85b` |
| gap_x2_source_plus_2 | 0.90 | 7.5 | 0.140987248 | 2.771736266 | `79d0cc164ceb9312` |

层距和光源距离各自增加时，理论宽度、实测灰度过渡宽度和灰度摘要均发生确定性变化。

## Forced-Kill Resume

命令流程：以 `DOME_TEST_BLOCK_DELAY_MS=80` 启动完整样本，在快照记录 5 块后发送
`SIGKILL -9`，再用相同参数重启。

```text
KILLED_AFTER_BLOCKS=5
recovery_count = 1
snapshot_used = true
resumed_block_ids = [0,1,2,3,4]
freshly_rendered_block_ids = [5..24]
```

与 `artifacts/sample/` 比较：

```text
rgba.png          MATCH
layer_map.png     MATCH
projections.json  MATCH
```

续算产物保存在 `artifacts/resume_full/`。

## Ctrl+C Resume

完整样本在 5 块提交后发送 `SIGINT`：

```text
SIGINT_EXIT_STATUS=0
recovery.status=interrupted
completed_block_count=5
snapshot.completed_count=5
```

随后同命令续算，RGBA、层归属图和投影清单与基线逐字节一致。产物保存在
`artifacts/ctrl_c_full/`。

## Snapshot Safety

自动化测试实际创建并处理了以下情况：

1. 将快照改成非法 JSON：整体丢弃，`snapshot_discarded=true`，从零重算，图像与基线一致。
2. 用同一输出目录渲染曝光不同的另一场景：场景摘要不一致，旧快照整体丢弃，结果与新场景全新渲染一致。
3. 修改已完成块 RGBA 载荷并重新计算块摘要、颜色摘要和快照校验，使快照“自洽”：恢复时逐通道重算
   发现差异，输出 `differences.json`，退出码 `5`。差异记录包含 `block_id`、`[x,y]`、
   `channel`、`stored`、`expected`。

## Determinism Notes

- 星点随机流是 SHA-256 计数器，不使用 `random` 或 NumPy RNG。
- 所有子像素和半影盘采样位置为固定闭式序列。
- 层内深度相同时按稳定的 `(kind, object id, triangle id, clip id)` 键决胜。
- 投影清单按几何质心与落点坐标规范化排序，不暴露三角面遍历次序。
- `report.json` 不含时间戳、绝对路径或耗时字段。
