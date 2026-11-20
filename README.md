# MeshEditor

面向 CAD 曲面网格协作的桌面编辑器与无界面回归工具。工程支持载入本地 OBJ/STL，执行顶点焊接、拓扑重建、选取变换、边折叠简化、撤销/重做，并按 OBJ v1/v2 或 STL ASCII/二进制导出。原网格作为校验真值，导出后重新载入会计算双向最近点偏差、法向夹角和分位数报告。

本项目从空工程实现，核心库不依赖 Qt；Qt 层仅负责 Widgets/OpenGL 交互和后台任务调度。因此无显卡或未安装 Qt 的环境仍可完成格式、拓扑、精度、并发与工程恢复的自动化验证。

## 构建

要求 C++17、CMake 3.16+、Qt 5.15 或 Qt 6.x（Widgets、OpenGL/OpenGLWidgets）。

```bash
# 桌面程序（找到 Qt 时自动构建）
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
./build/mesheditor

# 无 Qt / 无 GPU 的确定性核心和命令行回归
cmake -S . -B build-headless -DMESHEDITOR_BUILD_GUI=OFF -DCMAKE_BUILD_TYPE=Release
cmake --build build-headless -j
./build-headless/mesheditor_tests
./build-headless/mesheditor_cli regression .
```

Qt 6 在 CMake 中优先使用 `Qt6::Widgets` 与 `Qt6::OpenGLWidgets`，否则回退到 Qt 5 的 `Qt5::Widgets` 与 `Qt5::OpenGL`。缺少 Qt 时只输出警告并跳过 GUI target，核心、CLI 和测试仍可构建。程序启动时请求 OpenGL 3.3 Core，实际上下文版本写入应用日志。

## 主要目录

- `src/core/`：格式、网格、拓扑、焊接、版本、编辑历史、偏差校验、工程文件、任务门。
- `src/app/`：Qt Widgets/OpenGL 桌面编辑器。
- `src/cli/`：确定性样本、格式检查、转换、校验和脚本化回归入口。
- `tests/test_main.cpp`：不依赖 Qt 的核心回归测试。
- `samples/`：已生成的确定性 OBJ/STL 样本。
- `logs/`：轨迹日志、导出回读文件、偏差 JSON 和工程恢复样本。
- `scripts/run_regression.sh`：构建、测试、执行两次回归并逐字节比较。

## 格式和兼容规则

### OBJ

- 严格解析并保留 `v`、`vt`、`vn`、`f`、`g`、`usemtl`、`mtllib`、负索引和多边形面。
- 多边形使用确定性扇形三角化，并在转换记录中给出数量。
- 任一字段非法、索引越界或缺失时返回 `E_PARSE_INVALID`，错误结构包含行号；解析先写入临时 Mesh，失败不暴露部分数据。
- v2 使用每个 face corner 独立的 UV/法向索引，一个 group 可包含多个材质。
- v1 规则为共享 position 绑定一个 UV/法向组合，并且每个 group 只允许一个材质。降级前先做无损检查；无法表达时返回 `E_VERSION_LOSS`、列出受影响 group，并且不创建目标文件。
- 载入 v1 后在内存中自动升级为 v2 内部表示，解析和升级记录均写入转换轨迹。

### STL

- 自动识别 ASCII 和二进制 STL；二进制同时校验 84 字节头、三角形数量与文件长度。
- ASCII 解析严格检查 `facet normal`、`outer loop`、三条 `vertex`、`endloop`、`endfacet` 和 `endsolid`，错误包含行号。
- 写出时优先保留有效逐面法向，否则使用几何法向；二进制使用确定性 80 字节头和 float32，超出 float32 有限范围的坐标会被拒绝。
- STL 不承载 group/material/UV，导出成功但转换报告列出 `E_FORMAT_LOSS` 和所有受影响 group，不静默丢弃属性。

### 内部拓扑

焊接使用可配置几何容差和空间哈希，输出 `old/new/mergedVertices`、受影响面片数、退化面片数、旧顶点到新顶点的完整 merge map。焊接后重算法向并构建邻接、边面关系、边界边、边界环和非流形边统计。拓扑构建对重复顶点和退化面片做去重记账。

## 编辑与校验

- 视图支持轨道旋转、滚轮缩放、中/右键平移，以及投影到屏幕的 face/vertex 框选。
- 变换按 `scale -> rotate -> translate` 顺序围绕 pivot 执行；支持面片和顶点选择。
- 简化使用确定性最短边折叠，拒绝非二面边，移除退化/重复三角面，并压缩未引用顶点。
- 每个编辑记录操作前后顶点数、面片数、包围盒、体积估计、表面积和受影响面片数。
- 历史保存完整 Mesh 快照，撤销/重做恢复后 metric 与编辑前逐值一致。
- 校验对原载入网格和导出回读网格分别做双向采样、BVH 最近点查询和法向夹角统计。
- 报告包含 forward/reverse/combined 的 min/max/mean/RMS/p50/p90/p95/p99、metric 偏差、超差 truth/candidate face ID 和 PASS/FAIL。
- GUI 在失败时显示候选网格 overlay，并将超差候选面片标红；按 `Esc` 或“Clear validation overlay”返回当前编辑网格。

## 并发与工程文件

- 一个 `TaskGate` 管理当前网格的所有编辑、校验、导入、导出和工程读写；重复提交在入口返回 `E_BUSY`。
- Qt 后台任务使用 `QThreadPool`，主线程只负责提交快照和提交结果。取消令牌在焊接、简化、导出、回读和校验阶段检查。
- 取消或后台异常时立即释放 GPU VAO/VBO，下次绘制重建；文件流全部采用 RAII，任务结束即关闭。
- 工程文件保存当前网格、原始 truth 网格、undo/redo 完整历史、导出/版本设置、焊接容差和校验参数。
- 保存先写同目录临时文件，`fsync` 后原子 `rename`。
- 文件含 magic、工程版本、payload 长度和 FNV-1a 64 内容哈希。内容损坏或版本不匹配返回 `E_PROJECT_MISMATCH`；GUI 只在全部校验通过后替换场景，损坏工程不会覆盖当前网格。

## 业务码

| Code | 含义 |
| --- | --- |
| `E_PARSE_INVALID` | 格式/字段/索引错误，含行号 |
| `E_VERSION_LOSS` | 版本降级无法无损表达，含 group 列表 |
| `E_FORMAT_LOSS` | 格式本身不支持属性，作为转换报告项 |
| `E_BUSY` | 同一网格已有后台任务 |
| `E_PROJECT_MISMATCH` | 工程版本或内容哈希不匹配 |
| `E_CANCELLED` | 后台任务取消 |
| `E_IO` / `E_INVALID_ARGUMENT` | 文件系统或参数错误 |

## 样本和回归

样本生成器输出到 `samples/`：

- `cube_v2.obj`：12 面、8 顶点、3 group、3 材质、UV 和法向。
- `seam_version_loss.obj`：同一位置 UV 接缝并含两个材质，用于 `E_VERSION_LOSS`。
- `legacy_v1.obj`：v1 标记与顶点绑定属性，用于自动升级。
- `nonmanifold_degenerate.obj`：3 个面共享一条非流形边，并含退化三角形和重复顶点。
- `large_grid.obj`：64×64 网格，8192 个面，用于简化和大网格读取。
- `cube_ascii.stl`、`cube_binary.stl`、`nonmanifold_degenerate.stl`、`degenerate_ascii.stl`。

`mesheditor_cli regression .` 会在 `logs/` 输出：

- `regression_trace.jsonl`：生成、解析、v1 升级、E_VERSION_LOSS、焊接统计、OBJ/STL 回读偏差、编辑撤销/重做、简化、工程恢复哈希、损坏工程拒绝和 E_BUSY。
- `validation_report.json`：OBJ 导出后回读的双向偏差和分位数。
- `cube_reloaded.obj`、`session.mep`、`session_corrupt.mep`。

`scripts/run_regression.sh` 构建后执行两遍回归，分别写入工作区和 `.regression_second/`，再用 `diff -r` 验证样本与日志逐字节一致。

## 实际验证结果（2026-11-20）

- 环境：Ubuntu resolute、GCC 15.2、CMake 3.30.5、Qt 6.10.2、Mesa llvmpipe。
- GUI target 编译成功；smoke test 成功载入 `samples/cube_v2.obj` 并截图，OpenGL 实际上下文为 `4.5 (Core Profile) Mesa 26.0.8`，请求的 3.3 Core 生效，无 shader 错误。
- `./build-gui/mesheditor_tests`：`PASS 173 checks`。
- `./build-gui/mesheditor_cli regression .`：轨迹摘要 `PASS`。
- v2 OBJ 导出后回读：forward/reverse max 偏差、p95 与法向夹角均为 0，`passed:true`。
- ASCII/Binary STL 回读：在 `1e-6` 容差、density 3 下通过；float32 回读显示极小的舍入差异。
- 工作区与 `.regression_second/` 的 `samples/`、`logs/` 经 `diff -r` 比较为逐字节一致。
- 工程 round-trip 恢复 1 条历史记录；翻转 payload 字节后返回 `E_PROJECT_MISMATCH`。
- 非流形样本焊接合并 1 个重复顶点、移除 1 个退化面，并保留 1 条非流形边统计。
- 8192 面 `large_grid.obj` 以 density 4 做自校验耗时约 0.56 s、峰值约 15 MB；GUI smoke test 正常载入并渲染。

Qt 5.15 和 MSVC 构建路径已按兼容 API 编写，但本次容器只实际编译/运行了 Qt 6.10 + GCC 15；Qt 5/MSVC 未在本机验证。GUI 的交互截图和 OpenGL 上下文验证在软件渲染 llvmpipe 上完成，不代表特定 GPU 型号。
