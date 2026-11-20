# pcwb 架构与技术说明

## 1. 环境与 OpenGL 能力核实

| 项目 | 实测结果 |
| --- | --- |
| 系统 Python | `/usr/bin/python3` 3.14.4，无 pip/ensurepip |
| 受管解释器 | `uv 0.13.8` 提供 CPython 3.11.18，项目虚拟环境 `.venv` |
| 依赖 | numpy 2.4.6、PyOpenGL 3.1.10、PyOpenGL_accelerate 3.1.10 |
| 窗口库 | 系统无 `libglut`/freeglut，GLUT 的 `glutInit` 为 NullFunction，无法创建窗口 |
| OSMesa | 无 `libOSMesa` |
| EGL 离屏 | `libEGL.so.1` 可用；EGL 1.5 + pbuffer + OpenGL 4.5 Core（Mesa llvmpipe 26.0.8），`glClear`/`glReadPixels` 读回正确 |

按任务约定“无法创建窗口时以离屏渲染路径为准”，本项目主验证路径为
**EGL pbuffer 离屏 OpenGL 渲染器**，并附带零依赖的纯 NumPy CPU 渲染器兜底。
EGL 上下文用 ctypes 创建（PyOpenGL 的 EGL Python 包装存在调用签名缺陷），
上下文 current 之后的绘制全部走 PyOpenGL 绑定。

## 2. 模块结构

```
pcwb/
  errors.py        稳定错误码 E_POINTCLOUD_INVALID / E_COMMIT_FAILED / ...
  format.py        .pclb 二进制格式：80B 头 + 15B/点记录，CRC32 + SHA-256
  cloud.py         不可变 PointCloud 快照（数组只读）
  selection.py     矩形/球形/屏幕矩形选区，绑定云哈希，过期即 E_SELECTION_STALE
  operators.py     四个纯函数算子（crop/smooth/downsample/color_cover）
  versions.py      VersionStore：临时文件+fsync+os.replace 原子提交、清单、回退、哈希校验缓存
  diff.py          版本差异：新增/删除/颜色变化（坐标量化身份匹配）
  engine.py        单工作线程 EditEngine：协作取消、进度、一次性快照切换
  telemetry.py     环形缓冲帧延迟采样 + JSONL 确定性轨迹日志
  samples.py       种子驱动的确定性样本（噪声 + 球形空腔）
  app.py           Workbench 门面
  demo.py          脚本化确定性演示入口
  render/          camera / gl_renderer(EGL) / cpu_renderer / headless_loop / interactive(GLFW)
tests/             45 个 unittest 用例
```

## 3. 二进制格式（小端，format_version=1）

| 偏移 | 长度 | 字段 |
| --- | --- | --- |
| 0 | 4 | magic `PCLB` |
| 4 | 2 | format_version=1 |
| 6 | 2 | header_size=80 |
| 8 | 4 | point_count (uint32) |
| 12 | 24 | bbox：6 个 float32（min xyz, max xyz） |
| 36 | 40 | 保留零字节 |
| 76 | 4 | 头部前 76 字节的 CRC32 |
| 80 | 15×N | 每点 `3×float32 + 3×uint8` |

- 内容哈希 = 整文件 SHA-256，覆盖 CRC、bbox 与全部点字节。
- 校验：magic/版本/头长、CRC、文件长度 == 80+15N、坐标必须落在 bbox 内；
  任一不满足返回 `E_POINTCLOUD_INVALID`。
- 所有算术在 float64 进行，仅在序列化时一次性转 float32，保证跨平台逐字节一致。

## 4. 纯函数与确定性

- 算子只接受不可加快照与版本绑定选区，输出新数组与操作记录
  （算子名、参数、输入版本号/哈希、选中数、输出点数）。
- 确定性手段：float64 运算、`lexsort` 规范排序、体素键整数化、
  颜色 `np.rint` 后转 uint8、参数经 `_canonical` 归一为 JSON 安全值。
- 测试对每个算子重复执行并比较序列化字节哈希，确保逐字节一致。

## 5. 原子提交与版本一致性

1. 新云序列化到 `tmp/.stage-*`，`flush+fsync`；
2. `os.replace` 原子重命名到 `versions/vNNNNNN.pclb`；
3. 更新内存清单后，清单同样走临时文件 + 原子替换并 fsync 目录。

任一步失败：清理临时/半成品文件、回滚内存 head 与清单、保留旧版本字节，
返回 `E_COMMIT_FAILED`（测试用 mock 注入磁盘故障验证）。版本号在 store
生命周期内单调递增；清单是唯一事实来源，进程重开可完整恢复历史。

## 6. 渲染/后台线程隔离

- 渲染线程每帧只调用一次 `engine.current()` 拿到完整不可加快照，
  发布结果是锁内单次引用赋值，渲染线程不可能观察到半更新数据。
- 工作线程串行执行任务，提交前校验选区哈希与基准版本，过期则拒绝。
- 平滑算子每 256 点一个取消检查点、每 512 点上报进度；
  `CancelToken` 协作式取消，触发后停止计算、释放暂存缓冲，head 不变。
- 实测：6.8 万点云上对 3.7 万点执行 3 轮平滑耗时约 16.7 s，
  期间渲染线程照常产出 1051 帧（平均 5.7 ms/帧），旋转缩放输入不阻塞；
  取消请求在约 35 ms 内生效。

## 7. 回退、依赖失效与差异

- `checkout(v)` 将 head 移回历史版本并清空其它版本的内存缓存，
  后续编辑以该版本哈希为新父版本重新计算，杜绝复用分叉后的缓存。
- `load` 仅在缓存快照哈希 == 清单哈希时复用，篡改磁盘文件会被
  `E_COMMIT_FAILED`（哈希不符）拦截。
- 差异以坐标量化网格建立点身份（与索引顺序无关），输出新增/删除/
  颜色变化计数与各自索引，渲染器以绿/蓝/黄色高亮。

## 8. 遥测与可复现演示

- `LatencyMonitor` 定长环形缓冲记录每帧耗时与输入响应延迟，
  输出均值/最大值/p95；时钟可注入。
- 演示用确定性虚拟时钟，轨迹为 JSONL（canonical JSON，排序键、固定分隔符），
  事件含版本号、算子、参数、输入/输出哈希、帧耗时、ppm 哈希、取消、回退与差异。
- 相同种子与操作序列：样本、`versions/*.pclb`、`trajectory.jsonl`、
  `demo_summary.json` 逐字节一致；CPU 渲染帧逐字节一致，
  EGL/llvmpipe 帧跨独立 GL 上下文允许 ±1 量化抖动（测试按容差比较）。

## 9. 运行方式

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m pcwb.demo --workdir <可写临时目录>
# 强制 CPU 确定性渲染后端：
.venv/bin/python -m pcwb.demo --workdir <dir> --backend cpu
```
