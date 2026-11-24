# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）

环境：

- Go：`go version go1.22.12 linux/amd64`
- SQLite 驱动：`modernc.org/sqlite v1.34.5`（纯 Go，`CGO_ENABLED=0`）
- 样本：本地固定种子 `0x42A8691E` 生成，24 帧，192×108，4 通道

## 1. 单元与集成测试

命令：

```bash
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
```

实际结果：

```text
?       integritykernel/cmd/diagnose    [no test files]
?       integritykernel/internal/core   [no test files]
ok      integritykernel/internal/checkpoint     0.005s
?       integritykernel/internal/forensics      [no test files]
?       integritykernel/internal/samplegen      [no test files]
ok      integritykernel/internal/engine         1.843s
ok      integritykernel/internal/loader         0.802s
ok      integritykernel/internal/pipeline       1.766s
ok      integritykernel/internal/review         1.779s
ok      integritykernel/internal/watermark      0.099s
ok      integritykernel/internal/web            0.525s
```

覆盖的关键行为：

- 有效样本加载；清单缺失 `E_DECODE`；截断 PNG `E_DECODE` 且定位帧号/字段。
- 宽、通道、时间戳、帧数错误返回 `E_META`；水印区越界返回 `E_MARK`。
- 同一水印重复解码 JSON 完全一致；空白/低对比区域判为 missing；越界返回 `E_MARK`。
- `block_size=1` 返回 `E_PARA` 且字段为 `block_size`。
- 分块尺寸变更只失效 24 个噪声证据和 24 个判决证据；水印/残差缓存键不变。
- 阈值变更只失效 24 个判决证据。
- 水印区域修正只失效 24 个水印证据和 24 个判决证据。
- 单证据故障只导致该证据及其下游判决失败；清除故障后重试与全量结果一致。
- 三路比对通过；噪声差异可定位首个帧、块坐标和字段。
- SQLite 参数版本不匹配会清空旧快照。
- 独立复核可发现被篡改的持久化判决字段。
- HTTP 状态 API、参数调整 API 和诊断页面返回 200。

## 2. 增量 / 恢复 / 全量验收

命令：

```bash
./artifacts/integrity-diagnose crosscheck \
  -sample samples/default \
  -artifacts artifacts \
  -json artifacts/crosscheck.json
```

实际输出：

```text
state=initial_offset_region invalidations=0 inc_diff=false recovery_diff=false
state=corrected_region invalidations=48 inc_diff=false recovery_diff=false
state=block_size_12 invalidations=48 inc_diff=false recovery_diff=false
state=verdict_threshold_65 invalidations=24 inc_diff=false recovery_diff=false
crosscheck ok=true transaction=true review=true log_records=96 result=artifacts/crosscheck.json
```

核对结论：

- 增量路径与空缓存全量路径在四个参数状态下均无 `E_DIFF`。
- 断点恢复路径与全量路径在四个参数状态下均无 `E_DIFF`。
- 初始完整证据序列包含 `24 帧 × 4 证据类型 = 96` 条规范提交记录。
- 注入提交中断返回 `E_TXN`，`batch=2`；重启后发现的首个未完成批次同样为 `2`。
- 最终替换区间为 `13..16`，与真值 `replacement.start_frame=13`、`end_frame=16` 完全一致。

## 3. 普通核验命令

```bash
./artifacts/integrity-diagnose verify \
  -sample samples/default \
  -db artifacts/verify-checkpoint.db \
  -report artifacts/verify-report.json
```

实际输出：

```text
verified 24 frames; intervals=[{"start_frame":13,"end_frame":16,"confidence":0.9235299698287951,"frame_count":4}]
```

## 4. 独立复核

```bash
./artifacts/integrity-diagnose review \
  -db artifacts/recovery-checkpoint.db \
  -fingerprint samples/default/truth.fingerprint.json \
  -json artifacts/review.json
```

实际输出：

```text
review ok=true checked=24 intervals=[{"start_frame":13,"end_frame":16,"confidence":0.9235299698287951,"frame_count":4}]
```

该路径直接读取 SQLite 持久化证据和真值指纹，使用 `internal/review` 内自带的判决/区间重算逻辑，不调用 `engine` 或 `forensics` 主核验路径。

## 5. Windows 本地构建

命令：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -o artifacts/integrity-diagnose-windows-amd64.exe ./cmd/diagnose
```

已实际生成：

- `artifacts/integrity-diagnose`：Linux 本地诊断二进制，约 14 MB。
- `artifacts/integrity-diagnose-windows-amd64.exe`：Windows amd64 纯 Go 二进制，约 15 MB。

Windows PowerShell 推荐构建方式：

```powershell
$env:CGO_ENABLED="0"
go build -o integrity-diagnose.exe .\cmd\diagnose
.\integrity-diagnose.exe crosscheck -sample .\samples\default -artifacts .\artifacts
.\integrity-diagnose.exe serve -sample .\samples\default -artifacts .\artifacts -addr 127.0.0.1:8088
```

无需 Docker、C 编译器、数据库服务、外部账号或在线资源。

## 6. Web 界面冒烟

```bash
./artifacts/integrity-diagnose serve \
  -sample samples/default \
  -artifacts artifacts/smoke \
  -addr 127.0.0.1:18089
```

实测：

- `GET /` 返回 HTTP 200。
- `GET /api/state` 返回 24 帧报告。
- 报告区间为 `13..16`。
- 页面和脚本均由二进制内嵌静态文件提供，无外部 CDN 请求。
