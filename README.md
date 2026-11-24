# 采集点分布重建数据链路（collectionrecon）

完全离线、确定性的 Go 数据链路：固定种子生成地形区域与隐藏真值 → 装载校验 → 分层采样与密度
反推 → 确定性重建采集点 → 密度热力面 → 精确失效传播与增量/全量逐点比对 → SQLite 原子检查点
与断点恢复 → 本地 HTTP 诊断面板。

## 环境

- Go 1.22+（开发验证版本 go1.22.12）
- SQLite 纯 Go 驱动 `modernc.org/sqlite`，`CGO_ENABLED=0`，无 CGO/外部数据库依赖
- 仅标准库 + 本地嵌入静态资源，不访问外部网络

## 快速开始

```bash
export CGO_ENABLED=0
go test ./...
go run ./cmd/recon sample       -out artifacts/sample.json
go run ./cmd/recon run          -sample artifacts/sample.json -db artifacts/recon.db -out artifacts/report.json
go run ./cmd/recon incremental  -sample artifacts/sample.json -db artifacts/recon.db -zone 2
go run ./cmd/recon incremental  -sample artifacts/sample.json -db artifacts/recon.db -zone 1 -nocollect
go run ./cmd/recon recover      -sample artifacts/sample.json -db artifacts/recon.db
go run ./cmd/recon serve        -sample artifacts/sample.json -db artifacts/recon.db -addr 127.0.0.1:8080
```

Windows：

```powershell
$env:CGO_ENABLED=0
go test ./...
go build -o recon.exe ./cmd/recon
.\recon.exe run -sample sample.json -db recon.db
```

## 子命令

- `sample`：固定种子生成区域边界、地带、禁采区、地形栅格、采样格点、量化观测与隐藏真值。
- `run`：从空缓存全量重建，输出报告并写入分地带 SQLite 检查点，随后做真值容差验收。
- `incremental`：编辑地带参数或拖动禁采区，仅重算受影响地带，并与全量重建逐点/逐像素比对。
- `recover`：重启后校验内容哈希/参数版本/算法版本，匹配则复用，首个不匹配即 `E_VER` 全量重建。
- `serve`：本地诊断面板，区域俯视、采样与禁采区、重建点叠加、热力面、偏差上界曲线、失效示意。

## 错误码

`E_SCHEMA` 字段缺失 · `E_GEOM` 几何/栅格错误 · `E_OBS` 观测缺失 · `E_ZONE` 禁采区冲突 ·
`E_DEP` 失效传播误标 · `E_DIFF` 增量/全量不一致 · `E_BOUND` 偏差上界缺观测 ·
`E_VER` 检查点版本不匹配。

## 文档

详见 `docs/ARCHITECTURE.md`（架构、确定性设计、容差依据、八项要求映射与验证记录）。
