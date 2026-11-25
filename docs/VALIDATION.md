# 实际验证记录

验证日期：2026-11-25  
执行环境：Linux x86-64，Go 1.22.12；代码仅依赖标准库，路径与文件操作面向 Windows 10/11 也可用。

## 1. 构建与测试

```text
go build ./...
go test ./...
go test -race ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build ./...
```

结果：全部通过，Windows amd64 交叉编译也通过。测试覆盖输入校验、两种积分、量化单调性、`E_QMIX`、三路径一致、预算等待、版本失效、失败重试、预览/精确一致、色域定位、接缝定位、并发检查点写、独立验证器差异定位和本地 HTTP 页面。

自动化验收脚本 `scripts/validate.sh` 已执行通过。Windows PowerShell 版本为 `scripts/validate.ps1`。

## 2. 本地固定样本

```text
spectralforge sample --dir sample --seed 20261125
256x160, 4 meshes, 4 truth points, content=dde3fa4fb956
```

样本内容哈希：

```text
content  dde3fa4fb95617729fbabf4920a59a226ff0cc5f6f49872fea05a1e5ee648731
scene    ad728ffc44df79f4249c7a7188ec6474275c33892f0a5bc956f0b72d74f6c96a
spectral 107ccfcd7c859ec4e2742cd8e8878f843523a2940872789e5e646855464a8fde
```

样本规格：21 个波长采样点、4 条基函数、3 个逐波长光源、4 个网格、4 个解析真值像素。

## 3. 三条路径

| 路径 | 完成 | 新渲染块 | 复用块 | 提交序列 | 真值 |
| --- | --- | ---: | ---: | --- | --- |
| 一次性全量 | 是 | 40 | 0 | 0..39 | 通过 |
| 实时 | 是 | 40 | 0 | 0..39 | 通过 |
| 暂停 | 否 | 5 | 0 | 0..4，`E_PAUSED` | 不承诺完整帧 |
| 断点恢复 | 是 | 35 | 5 | 0..39 | 通过 |

帧摘要比较：

```text
linear_digest  c0fc2edffb01c40b532c29a8791e293cdc7ab5c82185b664e4cb0798627878e8
display_digest a7915d6b4a88169434f8fa0b2eda3769bbffc53974a9b29df7c8ad338ce798cf
pixels_equal   true
commit_sequence_equal true
```

三份 `frame.png` 的 SHA-256 完全一致：

```text
afcb9644e80ed2051e7ddbb9da1695a2a8ccbc290493fa18131ca91856e8fc08
```

## 4. 预算与性能

全量路径在 `max_workers=3`、`queue_budget=6` 下记录到 29 次 `queue_budget` 阻塞等待；等待原因和逐块归属均已持久化。

独立验证器结果：

```text
ok                 true
tiles_checked      40
pixels_checked     40960
commit_order_ok    true
main_render        0.122839 s
verifier           0.198748 s
main/verifier      0.6181
```

验证器未调用 `internal/render`，按块重新计算 40 个块、40960 个像素并全部通过。

## 5. 真值精度

4 个已知像素在完整渲染后均匹配生成器解析真值：

```text
mesh_vermilion  max ΔRGB = 0
mesh_jade       max ΔRGB = 0
mesh_cobalt     max ΔRGB = 0
mesh_gold       max ΔRGB = 0
```

峰值辐亮度与色度坐标同时输出并记录在 `frame.json` 的 `truth_results`。

## 6. 本地页面

实际启动：

```text
spectralforge serve --sample sample --checkpoints .validation/webcp --out .validation/webout --addr 127.0.0.1:18089
```

HTTP 验证结果：

```text
GET  /api/status               200
POST /api/exact                200, completed=true, truth_pass=true, 40 tiles
GET  /api/image.png?kind=exact 200, image/png, 4185 bytes
POST /api/verify               200, ok=true, commit_order_ok=true
```

页面精确渲染耗时 `0.140583 s`，独立验证耗时 `0.215669 s`。

显示参数预览复用验证：

```text
第一次预览：40 blocks rendered, 0 reused
修改曝光后第二次预览：0 blocks rendered, 40 reused
message: display-only retone: 0 spectral blocks recomputed
```

## 7. 异常路径

- 色域注入 `--inject-gamut 20:20:r:-0.5` 返回 `E_GAMUT`，字段包含 `x=20`、`y=20`、`channel=r`，且没有完成帧。
- 不匹配的 `--snapshot-version` 返回 `E_VER`，同时包含 expected/actual 版本。
- 单块一次失败注入后自动重试，`tile_attempts[7]=2`，最终帧与无失败路径逐像素一致。
- 采样版本变化后恢复会整目录作废并全量重算，报告 `invalidated=true`、`tiles_reused=0`。
- 并发块写测试和 `go test -race ./...` 均未发现数据竞争或 JSON 交错。

## 8. 可重复执行

```powershell
.\scripts\validate.ps1
```

或：

```bash
./scripts/validate.sh
```

脚本在 `.validation/` 下保留完整样本、三个检查点目录、帧、PNG、验证报告和比较报告。
