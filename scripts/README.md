# 本地样本与验收脚本

所有流程只读写本地文件和 SQLite，不访问真实账号、线上站点或容器。

## Linux / macOS / WSL

```bash
# 生成固定种子样本：samples/terrain-seed-42.json
./scripts/generate-sample.sh

# 运行全部测试与端到端验收
./scripts/acceptance.sh

# 启动诊断页面 http://127.0.0.1:18080
./scripts/run-server.sh
```

## Windows PowerShell

```powershell
# 需要 Go 1.22+；SQLite 使用 vendored pure-Go modernc.org/sqlite，
# 因此不需要 mingw-w64/gcc。
.\scripts\Generate-Sample.ps1
.\scripts\Invoke-Acceptance.ps1
.\scripts\Start-Diagnostics.ps1
```

## 手动命令

```bash
export CGO_ENABLED=0
go test ./...
go run ./cmd/terrain-gen -seed 42 -out samples/terrain-seed-42.json
go run ./cmd/terrain-acceptance -dir artifacts/acceptance
go run ./cmd/terrain-server -addr 127.0.0.1:18080
```

验收报告写入 `artifacts/acceptance/acceptance-report.json`，其中逐项记录
schema、真值、单调性、断裂归因、预算排队、增量、断点恢复、全量一致性、
版本作废和故障隔离结果。
