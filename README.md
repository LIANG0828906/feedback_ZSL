# Lumen Filter Studio

面向游戏与多媒体素材的本地图像滤波与增强桌面工作台。处理链固定为
`denoise → sharpen → local_contrast`，支持逐阶段参数、内容寻址缓存、
增量失效、原子快照、暂停/取消、强制终止后的前缀恢复，以及增量结果对
空缓存全量结果的逐像素审计。

![Windows 原生界面](artifacts/windows_ui.png)

## 已实现能力

- 三阶段确定性滤波：去噪、USM 锐化、分块局部对比度增强。
- `L / LA / RGB / RGBA` 多通道图像逐通道独立处理，输出尺寸、通道数与
  `uint8` dtype 不变。
- 参数范围严格校验，越界返回 `E_PARA`，包含阶段名与参数名。
- 缓存键由输入内容哈希、阶段标识、参数值、上游缓存键与阶段算法版本
  共同决定。
- 显式依赖图；参数变化只失效该阶段及其下游，上游缓存保留。
- 增量路径与空缓存全量路径自动逐像素比对，首个差异定位到阶段、`x/y`
  与通道；不一致返回 `E_DIFF`。
- 单文件 ZIP 快照原子提交，包含阶段输出、缓存键、参数、参数指纹、阶段
  版本、输入哈希与运行诊断。
- 输入哈希、参数指纹或阶段算法版本任一不匹配时，整个快照作废并全量
  重算，禁止新旧缓存混用。
- 协作式暂停/取消；取消时已提交阶段立即持久化；进程被强制终止后只从
  已持久化且版本匹配的阶段前缀恢复。
- 阶段失败不提交失败输出，不污染上游；可原地重试。
- PySide6 面板：原图/增强对照、差异热力图、几何真值轮廓、依赖图、缓存
  表、诊断统计、参数调节、导出增强图。
- 资源边界：单工作线程、严格阶段顺序、串行缓存提交、可配置工作集上限。

## 环境与安装

开发和验收使用以下环境：

- WSL2 / Linux：Python 3.14.4，NumPy 2.5.5，Pillow 13.0.0，
  PySide6 6.12.0，pip 26.3。
- Windows 10/11 原生：Python 3.10.9，NumPy 2.2.6，Pillow 12.2.0，
  PySide6 6.12.0，pip 22.3.1。

Windows PowerShell：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -e ".[test]"
```

Linux / WSL：

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e ".[test]"
```

本工作区已经准备了可用环境 `.venv/`；当前 `.venv/bin/python -m pip` 可用。
运行时不需要网络或外部服务。

## 启动与样本

仓库已包含 `samples/`：4 张确定性 PNG、4 份几何真值 JSON、清单与参数
序列。需要重新生成时：

```bash
python scripts/generate_samples.py samples
```

启动桌面工作台：

```bash
python -m filter_studio --samples samples --cache .cache
# 或安装后使用
filter-studio
```

Windows 原生启动冒烟：

```powershell
python scripts\windows_smoke.py
```

## 参数契约

| 阶段 | 参数 | 类型 | 范围 | 默认 |
|---|---|---:|---:|---:|
| `denoise` | `radius` | int | 0–5 | 1 |
| `denoise` | `strength` | float | 0.0–1.0 | 1.0 |
| `sharpen` | `radius` | int | 1–5 | 1 |
| `sharpen` | `amount` | float | 0.0–2.0 | 0.8 |
| `sharpen` | `threshold` | int | 0–255 | 2 |
| `local_contrast` | `tile_size` | int | 8–128 | 32 |
| `local_contrast` | `clip_limit` | float | 1.0–8.0 | 2.0 |
| `local_contrast` | `blend` | float | 0.0–1.0 | 0.75 |

算法版本在 `filter_studio/stages.py` 中显式定义；修改算法语义时应提升
对应 `StageSpec.version`。

## 缓存与恢复语义

缓存键：

```text
sha256({
  input_hash, stage, params, upstream_key, stage_version
})
```

首阶段 `upstream_key = "input:<content_hash>"`，后续阶段使用上游缓存键。
参数变化时，从最早发生变化阶段开始删除该阶段及全部下游条目；上游条目
和数组对象保持不变。

快照提交顺序为：临时文件写入 → ZIP 内容 fsync → `os.replace` → 父目录
fsync（Windows 跳过目录 fsync）。任一阶段完成后立即提交一次快照，因此
取消或强制终止最多丢失当前未完成阶段。

恢复时严格执行全有或全无的元数据校验：

- 输入像素内容哈希；
- 全参数指纹；
- 三阶段算法版本；
- 阶段必须构成合法前缀；
- 输出 dtype、shape 与输出哈希；
- 每阶段上游键和重新计算的缓存键。

任一项失败都会删除快照并清空内存缓存，随后全量重算。

## 验证

```bash
QT_QPA_PLATFORM=offscreen python -m pytest -q
python scripts/verify_acceptance.py
```

2026-11-23 的实际结果：

- pytest：`30 passed`。
- 验收脚本：`10 passed, 0 failed`。
- 4 种模式/4 种分辨率的增量结果均与空缓存全量逐像素一致。
- 只改 `local_contrast`：重算 1、命中 2；只改 `sharpen`：重算 2、命中 1。
- 强制终止子进程退出码 37，仅持久化 `denoise` 前缀；重启命中 1、重算 2，
  最终结果与全量一致。
- 注入首像素差异后返回 `E_DIFF`，定位
  `stage=local_contrast, x=0, y=0, channel=0`。
- Windows 原生 Qt 平台插件实际启动成功：`platform=windows`。

完整机器可读报告见 `artifacts/acceptance_report.json`，详细记录见
`VERIFICATION.md`。

## 目录

```text
filter_studio/
  filters.py       三阶段 NumPy 滤波算法
  stages.py        参数契约、版本、依赖图
  imaging.py       解码、哈希、热力图、差异定位
  cache.py         缓存键、不可变阶段输出
  snapshot.py      原子 ZIP 快照
  pipeline.py      调度、失效、审计、取消/恢复
  diagnostics.py   运行统计与比对报告
  samples.py       确定性样本与几何真值
  acceptance.py    端到端验收
  ui/              PySide6 窗口、控件、工作线程
samples/           PNG、几何真值、参数序列
tests/             30 项自动化测试
scripts/           样本生成、验收、Windows 冒烟
artifacts/         验收报告与界面截图
```

更细的模块设计、状态机与边界说明见 `TECHNICAL.md`。
