# 实际验证记录

验证日期：2026-11-24（Asia/Shanghai）

## 环境

```text
OS:        Linux WSL2 (Ubuntu 26.04 kernel)
Go:        go1.22.12 linux/amd64
Graphics:  当前 WSL 环境无 X11/OpenGL 开发头文件
Container: 未使用 Docker 或其他容器
```

本机无法安装系统级 X11 头文件（`sudo` 需要交互密码），因此：

- 核心管线、CLI、测试、PNG/JSON 导出全部在当前 Linux 环境实际运行；
- Fyne 面板使用官方 `ci` 软件驱动标签完成类型检查和编译；
- 使用 `GOOS=windows` 交叉编译了 CLI 与软件驱动 UI；
- 未声称已在 Windows 图形会话中实际打开窗口。Windows 原生运行命令见 README。

## 确定性样本

```bash
go run ./cmd/glyphbrush generate -out ./samples -seed 20261124
go run ./cmd/glyphbrush validate -samples ./samples
```

结果：

| 字形 | 轮廓 | 笔画段 | 扁平骨架点 | 真值包围盒 | 闭合面积 |
|---|---:|---:|---:|---|---:|
| `ui-a` | 2 | 6 | 98 | `[-313,-2]-[314,720]` | 264978.826084 |
| `orbit-o` | 2 | 8 | 194 | `[-328,80]-[329,543]` | 281346.112823 |
| `wave-s` | 2 | 12 | 315 | `[-292,36]-[281,638]` | 15348.983068 |

同一 seed 生成的两个 `glyphs.json`/`truth.json` 逐字节一致；不同 seed 轮廓不同。

## 单元与集成测试

```bash
go test ./...
```

输出（包名后均为 `ok`）：

```text
ok glyphbrush/cmd/glyphbrush
ok glyphbrush/internal/checkpoint
ok glyphbrush/internal/geom
ok glyphbrush/internal/loader
ok glyphbrush/internal/pipeline
ok glyphbrush/internal/raster
ok glyphbrush/internal/samplegen
ok glyphbrush/internal/stroke
```

并发竞态：

```bash
go test -race ./internal/pipeline ./internal/checkpoint ./cmd/glyphbrush
```

```text
ok glyphbrush/internal/pipeline
ok glyphbrush/internal/checkpoint
ok glyphbrush/cmd/glyphbrush
```

## 完整验收

```bash
go run ./cmd/glyphbrush verify \
  -samples ./samples \
  -out ./artifacts/verification \
  -checkpoints ./artifacts/verification/checkpoints
```

结果：`status = pass`，31/31 检查通过。

| 字形 | 网格 | 参数变更重绘 | 控制点变更重绘 | 控制点变更复用 | Composite SHA-256 |
|---|---|---:|---:|---:|---|
| `ui-a` | 671×766 | 6/6 | 3 | 3 | `c85751139fc05b50b9e3d640aba2243d0137c97851a46d8dc713a724a41c741d` |
| `orbit-o` | 701×507 | 8/8 | 4 | 4 | `75966f810265d576a6657e7167332b9f2da0dadffa626258ed455c16f0a658f2` |
| `wave-s` | 617×646 | 12/12 | 8 | 4 | `ab52892226844c28509e328bdebfd34ea9aebf0ec083f5970205c4d41c0b5805` |

每个字形都执行了：

- warm 全量渲染；
- 新引擎从检查点恢复并与 warm 结果逐像素/逐密度一致；
- `width += 6` 参数变更、增量渲染、空缓存全量渲染、自动比对；
- 控制点编辑、局部失效、增量/全量比对；
- `E_SELF`、`E_ZLEN`、`E_DENS`、`E_STAGE` 四类注入；
- 单段失败隔离、清除/重试、与全量一致；
- 参数版本不匹配时整快照作废检查。

## 异常与首个差异定位

| 场景 | 期望 | 实际 |
|---|---|---|
| 骨架注入自交 | `E_SELF` + glyph/segment/param | 通过 |
| 零长度段 | `E_ZLEN` + glyph/segment/param | 通过 |
| 密度溢出 | `E_DENS` + glyph/segment/param | 通过 |
| 单段内部失败 | `E_STAGE`；一次性故障重试成功 | 通过 |
| 人为翻转掩膜像素 | `E_DIFF` + 首个 `(x,y)` + 所属段 | 通过 |
| 人为修改密度采样 | `E_DIFF` + 采样像素 + 所属段 | 通过 |

异常测试确认：失败段之外的段仍为 `valid`，段数量不减少，复合结果只排除失败段；重试后恢复。

## 检查点

实际检查：

1. warm 渲染后，每个字形生成与笔画段数相同的 `segments/*.json`；
2. 同参数同算法同字形哈希的新引擎 `Restore` 成功，刷新结果与 warm 比对一致；
3. 仅改变参数版本后 `Restore` 返回 `checkpoint.ErrVersionMismatch`；
4. 该字形的整个快照目录被删除，禁止部分复用；
5. 部分提交（manifest committed 小于 segmentCount）加载时整体拒绝并删除；
6. `CommitAll` 全程持有存储互斥锁，按 ordinal 顺序提交。

## 构建产物

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o artifacts/bin/glyphbrush.exe ./cmd/glyphbrush

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -tags 'fyne ci' -trimpath -o artifacts/bin/glyphbrush-ui-ci.exe ./cmd/glyphbrush-ui
```

```text
artifacts/bin/glyphbrush.exe           3.8 MB
artifacts/bin/glyphbrush-ui-ci.exe      28 MB
```

Windows 原生 Fyne 运行仍应按官方文档安装 GCC/图形依赖，然后：

```powershell
go run -tags fyne ./cmd/glyphbrush-ui -samples .\samples
```

## 产物清单

```text
artifacts/verification/verification.json
artifacts/verification/latest.json
artifacts/verification/<glyph>/outline.png
artifacts/verification/<glyph>/bands.png
artifacts/verification/<glyph>/mask.png
artifacts/verification/<glyph>/density.png
artifacts/verification/<glyph>/summary.json
artifacts/verification/<glyph>/densities.json
artifacts/verification/<glyph>/segments.json
artifacts/verification/<glyph>/invalidation.json
```

`mask.png` 为灰度中心采样掩膜，重叠带呈现不同灰度；`density.png` 以 PPM 热度显示骨架采样点，并绘制左右边界点连线。
