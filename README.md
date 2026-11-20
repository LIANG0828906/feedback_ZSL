# Versioned Mesh Editor

本项目是一个本地桌面曲面网格编辑器，支持 OBJ、ASCII STL、二进制 STL 的解析与导出，
选区内变换、边折叠简化、顶点焊接、半边拓扑、双向几何偏差报告，以及以原子提交、
哈希链和磁盘快照为基础的版本恢复。

界面使用 PyQt5 + QOpenGLWidget。编辑在后台线程执行，渲染线程只读取最近一次已提交版本
的不可变快照；未提交结果不会改变界面、清单或已发布网格。

## 环境与渲染后端

本次实际验证环境：

- Linux/WSL2，Ubuntu 26.04.1 LTS
- Python 3.14.4
- numpy 2.5.5
- PyQt5 5.15 系列
- PyOpenGL 3.1.10
- 桌面通道：Wayland；`DISPLAY=:0`，`WAYLAND_DISPLAY=wayland-0`
- OpenGL：Mesa llvmpipe 软件渲染

本机 Qt 的 xcb 插件缺少若干 `libxcb-*` 运行库，因此实际窗口烟测使用
`QT_QPA_PLATFORM=wayland`。程序启动前会探测 OpenGL 上下文并读取 `GL_RENDERER`；
若驱动报告 `llvmpipe` / `softpipe` / `swrast`，会明确声明为软件渲染，而不是误报硬件渲染。

实际检测输出：

```text
RENDER_BACKEND=software; driver reports llvmpipe (LLVM 21.1.8, 256 bits)
UI_SMOKE_READY
```

如桌面 OpenGL 驱动异常，可强制软件渲染：

```bash
QT_QPA_PLATFORM=wayland python3 -m mesh_editor gui --backend software
```

若当前会话只能离屏运行：

```bash
QT_QPA_PLATFORM=offscreen python3 -m mesh_editor gui --backend software --smoke-test
```

Windows 上可安装 PyQt5/PyOpenGL 的本地可用 wheel；本项目运行代码只依赖
`numpy`、`PyQt5`、`PyOpenGL` 和 Python 标准库。

## 快速开始

```bash
# 1. 生成确定性 OBJ/STL 样本与版本基线
python3 scripts/generate_samples.py

# 2. 执行完整验收流程
python3 scripts/verify_acceptance.py

# 3. 运行单元/集成测试
python3 -m unittest discover -s tests -v

# 4. 启动编辑器
QT_QPA_PLATFORM=wayland python3 -m mesh_editor gui --backend auto
```

样本、版本仓库与验证产物位于 `data/`，日志和报告位于 `logs/`。

## 界面操作

- 鼠标左键拖动旋转，滚轮缩放；单击面片选择。
- `Shift` + 单击增减面片；`Ctrl` + 单击选择同组面片。
- 设置平移、旋转、缩放后点击 `Apply transform in background`。
- 设置目标面片比例后点击 `Simplify in background`。
- 编辑超过 `0.75s` 时出现进度对话框，可协作式取消；期间视口继续显示最近提交版本。
- `Undo last commit` 将当前头指针移回父提交，不重写任何快照版本号。
- `Rollback and recompute dependencies` 可回退到任意既有提交。
- 导出时可选择 OBJ/STL、v1/v2、ASCII STL、是否允许有损。
- 导出后可自动执行双向最近点与法向夹角校验，超差面片在视口标红，并写入 JSON 报告。

命令行也提供 `info`、`recover`、`export`、`transform`、`simplify`、`rollback`、
`undo`、`crash-after-stage` 和 `verify` 子命令。例如：

```bash
python3 -m mesh_editor info
python3 -m mesh_editor export --version 4 --format obj --format-version 2 --output out.obj
python3 -m mesh_editor recover
```

## 格式与版本规则

面片编号在错误和精度报告中均为 **0 基编号**。

### OBJ

- v2 是当前导出规则，写入 `# mesh_editor:format_version 2`，保留 `v`、`vt`、`vn`、
  面片分组和全局索引共享。
- v1 是可读的旧规则；载入后会升级到当前内部表示。v1 不能表示非默认面片分组，
  导出时会返回 `E_VERSION_LOSS` 并列出受影响面片，不写入目标文件。
- v1 与 v2 都使用 OBJ 原生索引，因此 OBJ 中的顶点共享不会因版本降级而在无提示的情况下丢失。

### STL

- 支持 ASCII 与二进制两种变体，二进制写入前会严格校验
  `84 + 50 * facet_count == file_size`。
- STL 无原生面片分组、UV 或顶点共享。导出存在这些属性时会返回 `E_VERSION_LOSS`
  并列出受影响面片；只有显式启用 `--allow-lossy` 或界面“Allow loss”才写出文件。
- 版本信息写入二进制 80 字节头或 ASCII `solid` 名称。

### 精确错误定位

- OBJ 字段缺失、索引越界、格式错误：`E_PARSE_INVALID`，附行号。
- 二进制 STL 长度矛盾：`E_PARSE_INVALID`，附字节偏移。
- ASCII STL 字段缺失：`E_PARSE_INVALID`，附行号。
- 降级有损：`E_VERSION_LOSS`，附面片列表。
- 导出哈希不一致：`E_VERSION_STALE`。
- 恢复链或快照损坏：`E_RECOVERY_INVALID`。

## 版本与恢复

版本仓库结构：

```text
data/workspace/
  manifest.json
  snapshots/
    v00000001/mesh.obj
    v00000002/mesh.obj
```

提交顺序：

1. 在 `snapshots/.staging-*` 写入完整网格并 `fsync`。
2. 将暂存目录原子重命名为新版本目录。
3. 将新清单写入同目录临时文件并 `fsync`。
4. 用 `os.replace` 原子替换 `manifest.json`。

清单中每个版本包含版本号、父版本、网格相对路径、内容哈希、容差、格式版本、
操作前后度量和依赖键。每个条目的 `entry_hash` 同时绑定前一提交的哈希，形成哈希链。

强制终止结果：

- 暂存阶段被杀：只留下 `.staging-*`，恢复时删除。
- 版本目录已发布但清单未替换：该目录是未提交孤儿，恢复时删除。
- 清单和网格均已提交：恢复校验全部条目和内容哈希后恢复到当前提交。
- 恢复不会为被终止编辑生成“退出报告”；报告字段固定为
  `exit_report_for_interrupted_edit: null`。

## 实际验证结果

`python3 -m unittest discover -s tests -v`：

```text
Ran 30 tests
OK
```

`python3 scripts/verify_acceptance.py`：

```text
all_pass: true
checks: 18 / 18 PASS
```

关键实测值：

- OBJ v2 往返：双向最近点最大偏差 `0`，超差面片 `0`。
- ASCII/二进制 STL 往返：双向最大偏差 `0`，并正确报告
  `E_VERSION_LOSS` 面片 `[0..13]`，属性为
  `face_groups`、`texture_coordinates`、`vertex_sharing`。
- 焊接：12 顶点合并为 11，`8 -> 0`；重复焊接报告逐字段一致。
- 非流形边：`[0,1]`、`[1,6]` 被拓扑层识别。
- 编辑序列：v1 baseline -> v2 transform -> v3 simplify；
  撤销后恢复 v2，度量逐值一致；回退到 v1 后新编辑生成 v4，依赖哈希来自 v1。
- 强制终止模拟退出码 `91`；恢复仍停在已提交 v4，删除 1 个暂存目录，
  中断编辑无退出报告。
- 故意截断网格：导出返回 `E_VERSION_STALE`，恢复返回 `E_RECOVERY_INVALID`。
- 篡改清单：恢复返回 `E_RECOVERY_INVALID`。
- 相同操作序列的 `manifest.json`、`trace.jsonl`、导出 OBJ 和版本快照逐字节一致。
- Wayland + llvmpipe 实际窗口烟测退出码 `0`。

完整结果：

- `data/verification/acceptance_report.json`
- `logs/verification.log`
- `logs/sample_generation.log`
- `data/verification/precision_obj_roundtrip.json`
- `data/verification/precision_stl_ascii.json`
- `data/verification/precision_stl_binary.json`
- `data/verification/precision_edited_stl.json`
- `artifacts/ui_window.png`（实际窗口截图）

## 代码结构

- `mesh_editor/formats.py`：OBJ/STL 解析、序列化、错误定位、版本损失报告。
- `mesh_editor/topology.py`：确定性顶点焊接、半边拓扑、边界环与非流形边。
- `mesh_editor/editing.py`：选区内变换、边折叠简化、编辑度量。
- `mesh_editor/versioning.py`：原子提交、清单哈希链、回退、恢复、依赖缓存。
- `mesh_editor/precision.py`：双向最近点、法向夹角、超差面片报告。
- `mesh_editor/samplegen.py`：确定性样本与基线生成。
- `mesh_editor/ui.py`：PyQt5/OpenGL 编辑器与后台编辑线程。
- `mesh_editor/acceptance.py`：可重复端到端验收流程。
- `scripts/`：样本生成和验收入口。
- `tests/`：30 项核心、版本、后台线程与恢复测试。

更完整的事务、线程和失效模型见 `TECHNICAL.md`。
