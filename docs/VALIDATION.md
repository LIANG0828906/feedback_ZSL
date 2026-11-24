# 实际验证记录

验证日期：2026-11-25（Asia/Shanghai）。

## 环境

- 工作区：`/home/liang/feedback/uid42a8691e-default-253`
- Go：`go version go1.22.12 linux/amd64`
- SQLite Go 驱动：`modernc.org/sqlite v1.29.10`，纯 Go，验证时使用 `CGO_ENABLED=0`
- 系统初始没有 `sqlite3` 命令；无密码 sudo 不可用。已用
  `apt-get download sqlite3` + `dpkg-deb -x` 解到临时目录验证：
  `3.46.1 2024-08-13 ... (64-bit)`
- 未使用 Docker 或其他容器；样本全部本地确定性生成。

## 自动化测试

命令：

```bash
go vet ./...
go test ./...
```

实际结果：

```text
VET_OK
?       audiocheck/cmd/audio-verify     [no test files]
?       audiocheck/cmd/audiocheck       [no test files]
?       audiocheck/internal/bound        [no test files]
?       audiocheck/internal/eval         [no test files]
?       audiocheck/internal/generator    [no test files]
?       audiocheck/internal/model        [no test files]
?       audiocheck/internal/pipeline     [no test files]
?       audiocheck/internal/store        [no test files]
?       audiocheck/internal/verify       [no test files]
ok      audiocheck                      0.113s
ok      audiocheck/internal/dashboard   0.004s
```

覆盖的验收点：

- schema 缺字段、不可整除、质量档位越界、序列截断。
- 指标重复评估逐字段一致、完美信号边界。
- 分块结果与整段单次评估一致。
- 独立验证器重算 SQLite payload。
- full / realtime / resume 三路径一致。
- 单块 `E_STAGE` 失败后只提交前缀，重试恢复后提交序列一致。
- 状态机合法/非法迁移与 `E_STATE` 保持原状态。
- `E_BAND` 块坐标和最小边带。
- `E_BOUND` 帧号和实测值。
- 内容/算法版本不匹配触发快照整体重置。
- worker 并行度、缓冲预算和队列背压等待记录。
- 验证器篡改定位到首个帧、块和字段。
- Dashboard HTML/API。

## 一键验收脚本

命令：

```bash
SQLITE3_BIN="<临时 sqlite3 解包路径>/root/usr/bin/sqlite3" \
  bash scripts/acceptance.sh
```

完整日志保存在：

- `artifacts/acceptance-20261125.log`

关键输出：

```text
generated .../sample: frames=12 samples/channel=416
truth=fef1e9bcdc421259f040bf41ded65024615532c60f5987f0e9da0dd678c97fec
mode=full blocks=6 frames=12 commit_order=[0 1 2 3 4 5]
mode=realtime blocks=6 frames=12 commit_order=[0 1 2 3 4 5]
observed expected E_STAGE rc=1
mode=resume blocks=6 frames=12 commit_order=[0 1 2 3 4 5]
ACCEPTANCE_OK
```

报告比较：

```json
{ "first_mismatch": null, "identical": true }
```

full vs realtime、full vs resume 均为 `identical: true`。

独立验证器：

```json
{
  "ok": true,
  "checked_blocks": 6,
  "checked_frames": 12,
  "commit_order": [0, 1, 2, 3, 4, 5]
}
```

SQLite CLI 查询恢复库：

```text
0|committed|1
1|committed|2
2|committed|3
3|committed|4
4|committed|5
5|committed|6
```

## 异常注入实测

```text
truncation   True  E_DECODE  original_samples  frame=7
gear         True  E_PARA    quality_gear
divisibility True  E_SCHEMA  frame_size
stage        True  E_STAGE   injected_stage_failure  frame=2 block=2
```

## 仪表盘冒烟

```bash
go build -o /tmp/audiocheck-server ./cmd/audiocheck
/tmp/audiocheck-server serve --report /tmp/audiocheck-accept-ft99/full.json \
  --addr 127.0.0.1:18088
curl http://127.0.0.1:18088/healthz
curl http://127.0.0.1:18088/api/report
```

实际结果：

```text
health: ok
api fields: 12 6 [0, 1, 2, 3, 4, 5]
html: 游戏音效编解码质量评估诊断台
```

## Windows 复现备注

Windows 上安装 Go 1.22+ 后确认：

```powershell
go version
$env:CGO_ENABLED = "0"
winget install SQLite.SQLite
sqlite3 --version
go test ./...
```

如果不使用 winget，可下载 SQLite 官方 `sqlite-tools-win-x64-*.zip` 并将
`sqlite3.exe` 所在目录加入 `PATH`。检查点文件可直接用：

```powershell
sqlite3 artifacts\demo\checkpoint.db ".tables"
sqlite3 artifacts\demo\checkpoint.db "select block_index,status,commit_seq from blocks order by block_index;"
```
