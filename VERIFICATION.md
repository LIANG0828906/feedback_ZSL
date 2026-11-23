# 实际验证结果

验证环境：

- 日期：2026-11-24（Asia/Shanghai）
- Go：`go1.22.12 linux/amd64`
- SQLite CLI：`3.46.1`
- 样本基准种子：`20261124`
- 工作线程：4
- 每线程缓冲：8

## 验收命令

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build ./cmd/workbench ./cmd/storygen
go run ./cmd/workbench demo \
  -samples samples \
  -artifacts artifacts/verification \
  -db data/verification.db \
  -saves data/verification-saves \
  -workers 4 \
  -buffer 8
```

## 端到端结果

```text
verification: PASS
  glass-meridian-20261124      boundaries=20/20 checkpoint=19 three_paths=true invalid=7/13 reused=6
  quiet-relay-20276962         boundaries=18/18 checkpoint=17 three_paths=true invalid=7/13 reused=4
  salt-orchard-20269043        boundaries=23/23 checkpoint=22 three_paths=true invalid=7/15 reused=7
  boundary checks=61 checkpoint shots=58
```

| 工程 | 镜头 | 时长(ms) | 边界续演 | SQLite 重启 | 三路径 | 失效节点/镜头 | 复用镜头 |
|---|---:|---:|---:|---|---:|---:|---:|
| `glass-meridian-20261124` | 19 | 16570 | 20/20 | PASS (19) | PASS | 7/13 | 6 |
| `quiet-relay-20276962` | 17 | 14220 | 18/18 | PASS (17) | PASS | 7/13 | 4 |
| `salt-orchard-20269043` | 22 | 21110 | 23/23 | PASS (22) | PASS | 7/15 | 7 |

`boundaries=shot_count+1`，包括 0 镜初始边界和全部终态边界。

## SQLite 检查

```text
project_id               save_schema_version  algorithm_version  last_shot_seq
-----------------------  -------------------  -----------------  -------------
glass-meridian-20261124  1                    story-engine/1     18

rows  first_shot  last_shot
----  ----------  ---------
19    0           18

integrity_check
---------------
ok
```

## 自动化测试

以下均通过：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `CGO_ENABLED=0 go build ./cmd/workbench ./cmd/storygen`（生成静态纯 Go 可执行文件）

覆盖点包括：

- `E_SCHEMA` 字段缺失、未知节点引用、重复 ID
- `E_PARA` 小数/负数时长、选择时间倒序
- `E_CYCLE` 因果环
- 每个镜头边界存档续演与连续推进逐镜一致
- 存档结构版本、算法版本、内容哈希和指纹冲突定位
- 旧存档冲突时不发生部分加载
- 编辑失效闭包、复用镜头、原子替换
- 多编辑序列重演
- `E_DIVERGE` 首个差异镜头序号、节点和字段定位
- SQLite 重启恢复、损坏行整体作废
- 6 路并发确定性提交保持 0..N-1 连续镜头
- 固定线程池结果顺序与最大并发限制
- HTTP 工程、比对、存档、检查点工作流
- 内嵌 UI 静态资源服务

## 诊断产物

完整机器可读结果与各路径产物位于：

- `artifacts/verification/verification-report.json`
- `artifacts/verification/verification-summary.md`
- `artifacts/verification/runs/compare-*`
- `artifacts/verification/checkpoint-*.db`

