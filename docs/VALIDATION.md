# 实际验证结果

验证日期：2026-11-21  
工作目录：`/home/liang/feedback/uid190c93df-default-33`

## 环境

| 项目 | 结果 |
| --- | --- |
| 系统 | WSL2 / Linux，浏览器 GUI 与 CPU 路径平台无关 |
| Python | 3.14.4（满足 >=3.11） |
| NumPy | 2.5.5 |
| PyOpenGL | 3.1.10 |
| OpenGL | OpenGL ES 3.2 Mesa |
| OpenGL renderer | llvmpipe (LLVM 21.1.8, 256 bits) |

Windows 专用提交路径使用 WGL 隐藏窗口检测。本次运行环境没有 Windows 原生
显示层，因此实际 OpenGL 验证走 EGL；Windows 分支已实现但未在当前机器执行。
浏览器 GUI、CPU 预览、转换、版本、恢复和导出均不依赖 Windows 图形 API。

## 自动化测试

```text
$ python -m pytest -q
....................................                             [100%]
36 passed
```

测试覆盖：

- 头部 CRC、载荷 CRC、长度、未知传递函数码、未知色彩码、非法位深、网格整除；
- 线性 / sRGB / PQ 往返、共享矩阵、白点适应、执行顺序无关；
- 单区块增量重算、全量复算哈希与误差摘要一致；
- 共享链变化导致全部区块失效；
- 版本 diff、历史回退、回退后提交父版本；
- `E_BUSY`、取消后旧版本完整；
- 峰值/曲线冲突、曝光溢出、参数表长度/键不一致；
- 未完成快照恢复、断链、字段缺失、缓存载荷损坏；
- 自动检查点记录未完成区块，取消后清除 pending 事务；
- 导出复载、哈希清单、报告篡改、陈旧版本拒绝；
- 同种子同操作序列的图集、报告、轨迹和快照逐字节一致。

## 端到端验收

```text
$ python scripts/run_acceptance.py
exit=0
```

完整机器可读结果：
`artifacts/acceptance/acceptance-report.json`。

结果摘要：

| 检查 | 结果 |
| --- | --- |
| 3 组确定性样本（Rec.709/sRGB/10-bit、P3/linear/12-bit、2020/PQ/10-bit） | PASS |
| 12 区块样本单区块修改 | PASS，只影响 tile 1 |
| 全量复算一致 | PASS，`exact_hash_match=true`，摘要最大差 `0.0` |
| 共享色彩链修改 | PASS，12 个区块全部受影响 |
| 强制中断续算 | PASS，复用 tile 0/1，仅重算 tile 2..11 |
| 回退后重新提交 | PASS，回退生成 v5，后续 v6 的父版本为 v5 |
| 非法峰值/曲线、曝光溢出、参数表不一致 | PASS，均返回 `E_PARAM_CONFLICT`，旧版本保留 |
| 导出复载 | PASS，版本 6，内容与报告哈希一致 |
| CPU/OpenGL 预览 | PASS，PNG 逐字节一致，CRC32 `78b91f0b` |
| 确定性 | PASS，图集/报告/轨迹/快照四个文件逐字节一致 |
| 浏览器 GUI API | PASS，首页/state/preview/submit/job/export/report 下载均成功 |

### 基线

12 区块生产样本的初始版本：

```text
version       = 1
chain_hash    = ddc39d385b9b9c3d6ed5079e3c8bf84d4d1e2e24a494437f778af016fd58fe2a
content_hash  = b97fdab08c6ec0376139c489a23de8459b51166aa1529f5984b5e5cf386fae43
```

### 最终导出

```text
version        = 6
atlas_sha256   = baeb29fc4bd5800704ddacd182f2e42173729c8c160ddda0635322b247642dcf
content_hash   = 1cd6ffd6b97071bb6df004de376a9102b4fcaf9685b08078cdcbde7f042bd1ac
report_sha256  = 3d8fdd32157cac3682aef8a821f2ca2b352bbdac9419527ded624d1cd5ad82c3
reload_verified = true
```

对应产物：

- `artifacts/acceptance/exported.atlas`
- `artifacts/acceptance/exported.report.json`
- `artifacts/acceptance/trace.json`
- `artifacts/acceptance/preview-cpu.png`
- `artifacts/acceptance/preview-opengl.png`
- `artifacts/acceptance/pending.snapshot.json`

### 非法参数定位

目标峰值 500 nit、源峰值 1000 nit、区块曲线 `none`：

```json
{
  "code": "E_PARAM_CONFLICT",
  "field": "tone_curve",
  "tile": 0,
  "details": {
    "target_peak_nits": 500.0,
    "source_peak_nits": 1000.0,
    "tone_curve": "none"
  }
}
```

tile 3 曝光 +4 EV、无压缩曲线：

```json
{
  "code": "E_PARAM_CONFLICT",
  "field": "exposure_ev",
  "tile": 3,
  "details": {
    "max_unclipped": 13.302328109741211,
    "tone_curve": "none"
  }
}
```

参数表少一个区块：

```json
{
  "code": "E_PARAM_CONFLICT",
  "field": "tile_params",
  "tile": 11,
  "details": {
    "actual": 11,
    "expected": 12
  }
}
```

三种拒绝后当前版本仍为 1，预览链路哈希与拒绝前一致。

## 确定性哈希

独立目录、相同种子、相同操作序列：

```text
atlas_sha256  = c422db8e1d9d1cb7db95719db59032e6e56d7d98230c392112b0001671f9a097
report_sha256 = 5f2d5efc5c800446dc15300381c2d82a05428f8f6a3eef636f02cecef59e28b2

out.atlas      identical = true
report.json    identical = true
trace.json     identical = true
snapshot.json  identical = true
```

## 复验命令

```bash
python -m pytest -q
python scripts/run_acceptance.py
python -m atlas_pipeline opengl-check
python -m atlas_pipeline verify-export \
  artifacts/acceptance/exported.atlas \
  artifacts/acceptance/exported.report.json
```
