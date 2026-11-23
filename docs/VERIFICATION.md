# 实测验收记录

记录时间：2026-11-24  
环境：Go 1.22.12, linux/amd64, 无第三方模块  
算法版本：`meshforge-repair/1.0.0`

## 1. 构建与单元测试

```text
$ go build -trimpath -o bin/meshforge ./cmd/meshforge
$ GOOS=windows GOARCH=amd64 go build -trimpath -o bin/meshforge.exe ./cmd/meshforge
$ go test ./...
?    meshforge/cmd/meshforge       [no test files]
?    meshforge/internal/errs       [no test files]
ok   meshforge/internal/generator
ok   meshforge/internal/mesh
ok   meshforge/internal/repair
ok   meshforge/internal/web
```

Windows 交叉构建成功，产物大小约 7.7 MiB；项目无 cgo 或平台专用依赖。

竞态检测：

```text
$ go test -race ./...
```

通过项覆盖：

- 诊断与真值逐项一致；
- 固定种子重复生成网格哈希与真值完全相同，样本 JSON 往返一致；
- 首个违规面片 `E_GEOM`；
- 容差过焊 `E_TOLO` 和建议容差；
- 非法平滑参数 `E_PARA`；
- Möbius 带朝向冲突 `E_ORIENT`；
- 质量超差 `E_SCALE`；
- 冻结阶段/非法重试 `E_STATE`；
- 检查点阶段恢复、参数版本失配整体失效、快照损坏整体失效；
- 精细/分块质量一致和重复指纹一致；
- 串行/并行最终哈希和阶段哈希一致；
- 回环地址白名单、HTTP 状态/网格接口、UI 运行接口。

## 2. 确定性样本

命令：

```text
./bin/meshforge generate --out data --seed 20261124 --levels 5,6,7
```

实际输出：

```text
generated small  level=5 vertices=4125  faces=8193   elapsed=92ms
generated medium level=6 vertices=16439 faces=32769  elapsed=315ms
generated large  level=7 vertices=65591 faces=131073 elapsed=1.495s
```

诊断缺陷：

| 样本 | 实际 seed | 非流形边 | 重复顶点 | 翻转面 | 朝向冲突边 | 边界环 | 开放链 | 分量 | 薄片 | 自相交对 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| small | 20266169 | 1 | 22 | 5 | 15 | 25 | 1 | 2 | 1 | 13 |
| medium | 20267178 | 1 | 48 | 5 | 15 | 51 | 1 | 2 | 1 | 26 |
| large | 20268187 | 1 | 48 | 5 | 15 | 51 | 1 | 2 | 1 | 49 |
| xlarge（可选，level 8） | 20269196 | 1 | 48 | 5 | 15 | 51 | 1 | 2 | 1 | 93 |

真值闭合曲面：

| 样本 | V | F | E | χ |
|---|---:|---:|---:|---:|
| small | 4,098 | 8,192 | 12,288 | 2 |
| medium | 16,386 | 32,768 | 49,152 | 2 |
| large | 65,538 | 131,072 | 196,608 | 2 |
| xlarge（可选） | 262,146 | 524,288 | 786,432 | 2 |

## 3. 修复运行

```text
$ ./bin/meshforge run --sample data/large --out runs/large
repaired large -> runs/large/repaired.json
hash=f6acfb6d844c9881a86d28ac5e1aa51f4ffecc51fdc715af0de2d57b9b9f25f9
mode=spatial
```

large 的 auto 决策记录：

```json
{
  "selected": "spatial",
  "rule": "exact is not admissible under declared limits; spatial chunk mode selected",
  "faces": 131073,
  "components": 2,
  "chunk": {
    "grid": 3,
    "edge": 1.272178287183778,
    "target_faces": 8192,
    "max_faces_per_chunk": 4855,
    "worker_buffer_faces": 79891,
    "boundary_overlap": 1
  },
  "candidates": [
    {
      "mode": "exact",
      "feasible": false,
      "reasons": [
        "faces 131073 > fine limit 50000",
        "components 2 <= fine limit 256",
        "estimated 5903ms <= 10000ms",
        "estimated 209MiB <= 384MiB"
      ]
    },
    {
      "mode": "spatial",
      "feasible": true,
      "reasons": [
        "estimated 4984ms <= 10000ms",
        "estimated 284MiB <= 384MiB"
      ]
    }
  ]
}
```

最终拓扑：

| 样本 | V | F | E | χ | 非流形边 | 边界环 | 重复顶点 | 翻转面 | 自相交 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| small | 4,098 | 8,192 | 12,288 | 2 | 0 | 0 | 0 | 0 | 0 |
| medium | 16,386 | 32,768 | 49,152 | 2 | 0 | 0 | 0 | 0 | 0 |
| large | 65,538 | 131,072 | 196,608 | 2 | 0 | 0 | 0 | 0 | 0 |
| xlarge（可选） | 262,146 | 524,288 | 786,432 | 2 | 0 | 0 | 0 | 0 | 0 |

## 4. 精细/分块质量一致性与并行确定性

命令：

```text
./bin/meshforge verify --data data --out artifacts/verification-all.json
```

实际输出：

```text
verified large  consistency=consistent=true fingerprint=d4048432c1263addd2a265baaa9502d2 fine=5576.5ms/157.0MiB spatial=5333.1ms/199.5MiB determinism=true
verified medium consistency=consistent=true fingerprint=3fb57040ad565c9bbd4e8970a880c86f fine=1127.4ms/46.5MiB spatial=1093.6ms/61.5MiB determinism=true
verified small  consistency=consistent=true fingerprint=5484f3f37e65c7b294821d4d89ed3e23 fine=281.9ms/13.2MiB spatial=294.3ms/16.2MiB determinism=true
```

质量项：

| 样本 | 项 | 精细 | 分块 | 差值 | 容差 | 结果 |
|---|---|---:|---:|---:|---:|---|
| small | vertices | 4,098 | 4,098 | 0 | 0 | pass |
| small | faces | 8,192 | 8,192 | 0 | 0 | pass |
| small | euler | 2 | 2 | 0 | 0 | pass |
| small | fill area | 0.0035487961826728884 | 0.0035487961826728884 | 0 | 3.548796e-9 | pass |
| medium | vertices | 16,386 | 16,386 | 0 | 0 | pass |
| medium | faces | 32,768 | 32,768 | 0 | 0 | pass |
| medium | euler | 2 | 2 | 0 | 0 | pass |
| medium | fill area | 0.0007621198548084271 | 0.0007621198548084271 | 0 | 1e-9 | pass |
| large | vertices | 65,538 | 65,538 | 0 | 0 | pass |
| large | faces | 131,072 | 131,072 | 0 | 0 | pass |
| large | euler | 2 | 2 | 0 | 0 | pass |
| large | fill area | 0.0002448362961229859 | 0.0002448362961229859 | 0 | 1e-9 | pass |
| xlarge | vertices | 262,146 | 262,146 | 0 | 0 | pass |
| xlarge | faces | 524,288 | 524,288 | 0 | 0 | pass |
| xlarge | euler | 2 | 2 | 0 | 0 | pass |
| xlarge | fill area | 0.00003963028675009486 | 0.00003963028675009486 | 0 | 1e-9 | pass |

串行/并行最终哈希：

| 样本 | serial hash | parallel hash | 相等 |
|---|---|---|---|
| small | `720c80f67f1d3ec48c8f0cc0d5bddd0c777d8d3011e58e17130d6866236db68b` | 同左 | 是 |
| medium | `4ab06ac35b3296cd82ceeaec48e9bebd90f5380e080dcb1939de1e8596908e13` | 同左 | 是 |
| large | `f6acfb6d844c9881a86d28ac5e1aa51f4ffecc51fdc715af0de2d57b9b9f25f9` | 同左 | 是 |
| xlarge | `789b1d10fa3f5fb7d79b7d1c6284aff0c96e2b440240b9938a09f51d60b885a4` | 同左 | 是 |

同一输入重复执行 `RunConsistency` 得到相同 `quality_fingerprint`；时间/堆采样是观测字段，不进入指纹。

## 5. 524,288 面规模上限验证

为了覆盖“数十万面片”上限，额外生成 level 8 样本，并以 `--time-budget-ms 60000 --memory-budget-mb 2048` 执行双模式验证：

```text
generated xlarge level=8 vertices=262199 faces=524289 elapsed=5.714s
repaired xlarge -> /tmp/meshforge-xl-run/repaired.json hash=789b1d10... mode=spatial
verified xlarge consistency=consistent=true
  fingerprint=8024e76a4d7e858738ee602e4f24539c
  fine=27257.9ms/720.9MiB
  spatial=26999.3ms/935.0MiB
  determinism=true
```

分块决策：`grid=5`、`edge=0.7845`、`max_faces_per_chunk=4195`、`worker_buffer_faces=79891`。精细/分块最终均为 `V=262146, F=524288, E=786432, χ=2`，孔洞填补面积差值为 0；串行与并行哈希均为 `789b1d10fa3f5fb7d79b7d1c6284aff0c96e2b440240b9938a09f51d60b885a4`。

## 6. 检查点

large 完整运行后有：

```text
manifest.json                   1931 bytes
stage-00-diagnose.json          6692867 bytes
stage-01-weld.json              6690624 bytes
stage-02-fill.json              6691980 bytes
stage-03-orient.json            6693013 bytes
stage-04-denoise.json           6727827 bytes
stage-05-stats.json             6729896 bytes
```

再次执行同一命令：

```json
{
  "resume": "resumed_stage_5",
  "progress": {"committed": 6, "total": 6},
  "output": "f6acfb6d844c9881a86d28ac5e1aa51f4ffecc51fdc715af0de2d57b9b9f25f9"
}
```

单元测试还验证：

- 只提交 diagnose+weld 后重启，从 fill 继续，最终哈希与全量运行相同；
- 修改平滑参数后重启，发现参数版本不匹配，`resume_status=invalidated`、进度归零；
- 破坏任一阶段快照后重启，发现内容/解码异常，整链作废、进度归零；
- 已提交阶段重跑返回 `E_STATE`。

## 7. Web 回环验证

启动：

```text
./bin/meshforge serve --sample data/small --run runs/small --addr 127.0.0.1:18787
meshforge UI listening on http://127.0.0.1:18787
```

状态接口：

```json
{
  "sample_id": "small",
  "progress": {"committed": 6, "total": 6},
  "mode": "exact",
  "resume_status": "resumed_stage_5",
  "final_stats": {
    "vertices": 4098,
    "faces": 8192,
    "edges": 12288,
    "euler_characteristic": 2,
    "non_manifold_edges": 0,
    "boundary_loops": 0,
    "duplicate_vertices": 0,
    "self_intersections": 0
  }
}
```

网格接口：

```text
original: 4125 vertices, 8193 faces, 12 highlighted faces
repaired: 4098 vertices, 8192 faces, χ=2
```

HTML 包含 3 个 Canvas（原网格、修复网格、拓扑曲线）。对已提交的 diagnose 阶段调用 retry 返回 HTTP 409：

```json
{"code":"E_STATE","message":"only a failed stage can be retried; diagnose is committed"}
```

`ValidateLoopback` 测试拒绝 `0.0.0.0:8787`、`:8787` 和局域网地址，允许 `127.0.0.1`、`[::1]`、`localhost`。

## 8. 可复现产物

- 全量验证：`artifacts/verification-all.json`
- small 单独验证：`artifacts/verification-small.json`
- large 单独验证：`artifacts/verification-large.json`
- xlarge 单独验证：`artifacts/verification-xlarge.json`
- large 运行与检查点：`runs/large/`
- 样本与真值：`data/small`、`data/medium`、`data/large`

生成本地数据的命令全部离线；测试和运行过程未下载任何模块。

