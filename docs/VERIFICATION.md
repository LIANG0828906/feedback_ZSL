# 实际验证记录

环境（本机实测）：

* Ubuntu 26.04 (WSL)，系统 Python 3.14.4（>= 3.10 满足）；系统无 pip/venv，
  通过 `get-pip.py --break-system-packages` 安装 pip + virtualenv 后创建项目虚拟环境 `.venv`；
* 依赖版本：numpy 2.5.5、PyQt5 5.15.14、PyOpenGL 3.1.10（本机 pip 在线安装，无外部账号/服务）；
* OpenGL：`npr.cli gl-probe` 实测
  `{"gl": true, "platform": "wayland", "version": "4.5", "offscreen_fallback": true}`；
  `QT_QPA_PLATFORM=offscreen` 同样可创建上下文；xcb 平台插件在本机不可用，
  预览窗口与探测逻辑均已实现自动回退离屏光栅化。

## 自动化测试

` .venv/bin/python -m unittest discover -s tests` → `Ran 40 tests ... OK`。

| 测试文件 | 覆盖业务要求 |
| --- | --- |
| `tests/test_scene.py` | 要求 1：闭合/断链/非有限/分区重叠、贝塞尔展平、错误定位 path 编号 |
| `tests/test_strokes.py` | 要求 2：三层生成、剔除编号、密度驱动 alpha、同参数逐字节一致 |
| `tests/test_pipeline_storage.py` | 要求 3/4/8：网格路由、失效闭包、批次复用、提交失败回滚、哈希链断裂、轨迹字段 |
| `tests/test_preview_concurrency.py` | 要求 6/7：快照一致性、`E_STALE_VERSION`、`E_BUSY`、取消保留旧版、发布竞态 |
| `tests/test_hash_chain.py` | 要求 5：跨版本父清单不可变哈希链与断链报错 |
| `tests/test_crash_recovery_integration.py` | 要求 5：独立子进程 `os._exit(9)` 模拟 SIGKILL 后仅依据磁盘续算 |
| `tests/test_samples_cli.py` | 要求 8：生成器/回归可复现、轨迹逐字节一致、PNG 输出 |

## 失效与复用实测（rundir/run_medium/trace.jsonl）

操作序列：v1 初始构建 → v2 同参数空操作 → v3 仅改轮廓宽度 → v4 仅改阴影间距。

| 版本 | 轮廓 | 阴影 | 涂抹 | 实测现象 |
| --- | --- | --- | --- | --- |
| v1 | 重算 2 批 | 重算 3 批（剔除 19） | 重算 2 批 | 全量构建 |
| v2 | 复用 2 批 | 复用 3 批 | 复用 2 批 | 无因子变化，零重算 |
| v3 | 重算 2 批 | 重算 3 批 | 重算 2 批 | 轮廓变化经剔除闭包传导全层 |
| v4 | 复用 2 批 | 重算 4 批（剔除 19） | 重算 2 批 | 仅阴影+涂抹失效，轮廓哈希复用 |

large 样本同序列下 v1 阴影剔除 63 条、v4 剔除 82 条，剔除编号写入清单与轨迹。

## 确定性实测

* 两个独立工作目录分别执行 `npr.cli regression`：`baseline.json` 与全部
  `trace.jsonl` `diff` 结果逐字节一致（`BYTE_IDENTICAL`）；
* 同版本预览 PNG 在两次独立运行中字节完全相同（20049 字节，画布 SHA-256 一致）；
* 轨迹行格式为 `<16位内容前缀> <canonical JSON>`，字段含版本号、层号、批次号、
  受影响/复用批次数、分块数、被剔除笔触数与内容哈希，无时间戳/文件大小字段。

## 强制终止与续算实测（npr.crash_demo）

1. 先提交完整 v1；
2. 子进程执行 `crash`，在 v2 提交循环进入阴影层批次时 `os._exit(9)`（等价 SIGKILL，无清理、无退出报告）；
3. 磁盘上 `manifest.json` 仍只指向 v1，遗留未提交的 `chunks/v2/`；
4. 另一进程执行 `resume`：哈希链校验通过 → 识别并丢弃 v2 未提交分块 →
   按 `pending_params.json` 续算出完整 v2（三层齐备），输出
   `{"recovered_version": 1, "completed_version": 2, "layer_count": 3}`；
5. 再次 `npr.cli recover` 返回 `recovered complete version v2`。

## 预览一致性实测

* `PreviewService.publish` 在独立 numpy 缓冲完成整版合成后一次性交换快照；
  200 次并发读取与 5 次连续发布的竞态测试中未观察到任何半更新层集合；
* 发布后持有旧快照句柄调用 `verify_handle` 稳定返回 `E_STALE_VERSION`；
* PyQt5 窗口以 500ms 定时器读取当前快照，离屏平台实测可正常显示 v4；
  预览图见 `rundir/preview_medium.png`、`rundir/preview_large.png`。

## 复现实验命令

```bash
.venv/bin/python -m unittest discover -s tests
.venv/bin/python -m npr.cli gl-probe
.venv/bin/python -m npr.cli gen-samples rundir/samples
.venv/bin/python -m npr.cli regression rundir
.venv/bin/python -m npr.cli preview rundir/run_medium rundir/preview_medium.png
.venv/bin/python -m npr.crash_demo crash  rundir/crash_demo rundir/samples/scene_medium.json 1 2
.venv/bin/python -m npr.crash_demo resume rundir/crash_demo rundir/samples/scene_medium.json
```
