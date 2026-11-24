# 实际验证记录

日期：2026-11-25  
环境：WSL2 Linux x86-64，Go 1.22.12，`CGO_ENABLED=0`，SQLite 驱动 `modernc.org/sqlite v1.34.5`

## 执行的命令

```bash
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -trimpath -o artifacts/vcomp ./cmd/vcomp
./artifacts/vcomp gen --out samples --seed 42
./artifacts/vcomp verify --data samples --work artifacts/verify-final6
./artifacts/vcomp workbench --data samples --version v3 \
  --db artifacts/wb3.db --project wb3 --demo --out artifacts/wb-demo3
```

## 测试结果

```text
?   	virtualstudio/cmd/vcomp	[no test files]
ok  	virtualstudio/compose
ok  	virtualstudio/engine
ok  	virtualstudio/formats
ok  	virtualstudio/model
ok  	virtualstudio/sample
ok  	virtualstudio/store
```

`go vet ./...` 无报告。

## 验收矩阵

`vcomp verify` 的 19 项检查全部通过：

- `truth v1`、`truth v2`、`truth v3`：采样像素、逐帧时间戳、可见性终点与帧哈希一致。
- `cross-version canonical stack`：三版本归一后的层标识、顺序、可见性、不透明度、像素与时间基准一致。
- `repeat composition determinism`：同帧重复合成逐像素一致。
- `loader error contracts`：缺失/未知版本、V3 引用环、损坏 base64、越界参数与采样点均返回规定错误码和字段路径。
- `window state machine`：非法迁移返回 `E_STATE` 且状态不变。
- `full baseline`、`realtime path`、`resume restart`：三条路径的帧、统计和窗口提交序列一致。
- `resume first segment`：在 2 个窗口提交后取消，状态为 `K K P P P P`；重启仅恢复版本匹配窗口。
- `incremental invalidation`：`prop` 不透明度编辑只失效窗口 `[1,2,3,4]`，层清单位为 `[prop]`，三个导出阶段均失效。
- `incremental equals full`：增量与空缓存全量逐像素、逐统计、逐提交一致；窗口 0、5 复用持久化缓存。
- `resource buffer queue`：总像素缓冲不足时 3 次排队事件被记录，结果仍与全量一致。
- `window failure isolation` / `window retry recovery`：窗口 1 注入 `E_DECODE` 后仅窗口 0 提交；重跑后恢复且结果一致。
- `cross-version export/downgrade`：V3→V1 生成预乘与层级路径的 `E_LOSSY` 清单，重复导出逐字节一致，回读层栈一致。
- `checkpoint poison detection`：篡改一个窗口内容哈希后整体快照作废并全量重算，未复用任何旧窗口。

完整机器可读报告：`artifacts/verify-final6/verify-report.json`。

## 工作台演示结果

演示依次执行查看层栈、窗口矩阵、第 4 帧、编辑 `prop` 不透明度、隐藏 `character`、第 10 帧差异热力图、导出。

关键输出：

```text
invalidated windows=[1 2 3 4] layers=[prop] exports=[export:v1 export:v2 export:v3]
run mode=realtime progress=6/6 commits=[0 1 2 3 4 5] reused=[0 5] recomputed=[1 2 3 4]
diff frame 10: max=103 mean=6.6361 mismatches=695
```

生成文件：`artifacts/wb-demo3/diff_frame_0010.png`。V3→V1 导出同时打印 `order_expression` 和三个 `source_alpha_mode` 的 `E_LOSSY` 记录。

## Windows 构建验证

已在当前 Linux 宿主交叉构建：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -o artifacts/windows/vcomp.exe ./cmd/vcomp
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o artifacts/windows/tests/virtualstudio_engine.test.exe ./engine
```

`vcomp.exe` 与全部 6 个有测试包的 `.test.exe` 均成功生成，格式确认为 `PE32+ executable for MS Windows ... x86-64`。

当前 WSL 宿主未安装可调用的 Windows `go.exe`、PowerShell 或 Wine，因此无法在本机真正执行 Windows 测试二进制。Windows 主机上请运行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\windows-build-test.ps1
```

该脚本执行 `go test ./...`、`go build`、生成样本、运行完整验收与工作台演示，并将断点恢复数据库保留在 `artifacts/windows/workbench.db`。

