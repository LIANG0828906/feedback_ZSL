# Local Verification Record

运行日期：2026-11-24  
环境：Ubuntu 26.04, Go 1.22.12, Wails CLI v2.10.2, 8 GB RAM, 无 Docker

## 1. Unit and race tests

```bash
go test ./...
go test -race ./internal/...
go test -tags wails . -run TestWailsAPI
go vet ./...
```

结果：全部通过。测试覆盖：

- 固定种子样本生成与校验
- 串行/并行逐点一致
- 密度、点数、稀有度、热力面容差
- 未受影响地带缓存复用与依赖下游失效
- 禁采区编辑与 `E_SAMPLE`
- 增量结果与空缓存全量重建比较
- 隐藏真值扰动不改变估计结果
- `E_SCHEMA` / `E_GEOM` / `E_SAMPLE` / `E_BIAS` / `E_DIVERGE`
- SQLite 恢复、参数版本篡改导致整快照失效
- Wails 绑定 API 初始化、增量编辑、全量比较和报告导出

## 2. Deterministic sample

```bash
go run ./cmd/workbench sample -seed 42 -out samples/verdant-reach.json
go run ./cmd/workbench validate -region samples/verdant-reach.json
```

实测：

```json
{
  "ok": true,
  "region_id": "verdant-reach",
  "zones": 3,
  "sample_sites": 40,
  "truth_points": 1642,
  "raster": {"rows": 12, "cols": 16}
}
```

## 3. Truth tolerance check

```bash
go run ./cmd/workbench truth-check \
  -region samples/verdant-reach.json \
  -db /tmp/rw-verify.sqlite
```

实测摘要：

| Zone | Density error | Rarity L1 | Count rel. error | Heatmap error |
| --- | ---: | ---: | ---: | ---: |
| zone-1-west | 0.00019 | 0.00000 | 0.00208 | 0.00098 |
| zone-2-northeast | 0.00015 | 0.00000 | 0.00231 | 0.00088 |
| zone-3-southeast | 0.00008 | 0.00000 | 0.00000 | 0.00098 |

声明的密度容差为 `0.02`，点数相对容差为 `0.02`，热力面容差为 `0.02`，均通过。

## 4. Incremental equals full

```bash
go run ./cmd/workbench edit-zone \
  -region samples/verdant-reach.json -db /tmp/rw-verify.sqlite \
  -zone zone-1-west -density-limit 0.09

go run ./cmd/workbench compare \
  -region samples/verdant-reach.json -db /tmp/rw-verify.sqlite
```

实测 `comparison.equal=true`，`checked_items=63`。增量路径只重算了 `zone-1-west`；东北和东南地带复用缓存；最终提交序仍为：

```text
zone-1-west, zone-2-northeast, zone-3-southeast
```

`go test` 中的 `TestDivergeErrorIdentifiesFirstZoneAndItem` 主动篡改首个地带点集，验证比较返回 `E_DIVERGE`，`zone_id=zone-1-west`，`data_item=points`。

## 5. SQLite recovery

`TestSQLiteResumeAndSnapshotInvalidation` 验证重启后所有地带从检查点恢复且 `recomputed` 为空。  
`TestSQLiteResumesMissingZoneOnly` 删除一个 current 地带检查点，验证仅补算缺失地带、其余缓存继续复用，最终点集不变。  
`TestSQLiteMismatchInvalidatesWholeSnapshot` 篡改一个 current 地带的 `param_version`，验证重启后：

- `snapshot.invalidated=true`
- 所有地带重新计算
- 重建点集与首次运行逐点一致

## 6. Bias failure path

`TestBiasError` 把 `bias_threshold` 调至 `0.00001`，验证：

- 返回 `E_BIAS`
- 报告包含首个超限地带
- 错误对象包含 `zone_id`、`data_item`、`value`

## 7. Wails environment

```bash
wails doctor
```

实测关键项：

- Wails v2.10.2
- Go 1.22.12
- Linux amd64
- 容器缺少 `pkg-config`、`libwebkit2gtk`，因此该容器不能完成 Wails 原生窗口打包/启动
- 前端为零 npm 依赖静态资源

```bash
go build -tags wails .
```

实测通过，验证 Wails 绑定与应用入口可编译。`TestWailsAPIInitializeEditAndCompare` 进一步在不启动原生窗口的情况下验证初始化、参数编辑、全量比较和报告导出。

Windows 本地启动条件与命令：

```powershell
# 先确认 WebView2 Runtime 已安装
wails doctor
wails dev -tags wails
wails build -tags wails
```

Linux 如需原生窗口：

```bash
sudo apt install pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
wails build -tags wails
```
