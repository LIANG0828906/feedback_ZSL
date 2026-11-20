# pcwb — 版本化本地点云工作台

面向本地三维扫描点云的浏览与编辑工作台：自定义二进制格式读写、矩形/球形
选区、裁剪/平滑/降采样/着色四个纯函数算子、版本化快照与原子提交、后台线程
编辑与渲染线程隔离、协作式取消、版本差异可视化、确定性样本与可复现轨迹日志。

## 环境准备

本机系统 Python 3.14 不带 pip，使用 `uv` 管理的 CPython 3.11.18：

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

OpenGL 能力：本机无 freeglut/OSMesa（无法创建窗口），但 EGL 1.5 + Mesa
llvmpipe OpenGL 4.5 离屏上下文可用，因此按任务约定以**离屏渲染路径验收**；
另提供零依赖 CPU 渲染器兜底，GLFW 交互窗口为可选模块。

## 运行

```bash
# 全量测试（45 个用例）
.venv/bin/python -m unittest discover -s tests -v

# 脚本化确定性演示：样本、6 个版本、取消、回退、差异、帧与轨迹日志
.venv/bin/python -m pcwb.demo --workdir <可写临时目录>
.venv/bin/python -m pcwb.demo --workdir <dir> --backend cpu   # 纯 CPU 逐字节渲染
```

演示产物：

```
<workdir>/
  samples/                 确定性样本 + 基线哈希清单
  store/manifest.json      版本清单（父版本、输入/输出哈希、操作记录）
  store/versions/v*.pclb   不可变版本文件（临时文件 + 原子重命名提交）
  frames/*.ppm             离屏渲染帧（选区/差异高亮）
  trajectory.jsonl         确定性轨迹日志（逐字节可复现）
  demo_summary.json        版本谱系、差异计数、帧耗时统计
```

## 代码地图

| 模块 | 职责 |
| --- | --- |
| `pcwb/format.py` | `.pclb` 二进制读写、CRC32、SHA-256、越界/格式校验 |
| `pcwb/cloud.py` | 不可变点云快照（数组只读） |
| `pcwb/selection.py` | 矩形/球形/屏幕矩形选区，绑定版本哈希 |
| `pcwb/operators.py` | 四个纯函数、逐字节确定的编辑算子 |
| `pcwb/versions.py` | 原子提交、清单、回退、哈希校验缓存 |
| `pcwb/engine.py` | 后台工作线程、取消令牌、进度、一次性快照切换 |
| `pcwb/diff.py` | 版本差异（新增/删除/颜色变化） |
| `pcwb/render/` | EGL OpenGL 渲染器、CPU 渲染器、轨道相机、离屏帧循环 |
| `pcwb/telemetry.py` | 帧延迟环形采样与 JSONL 轨迹日志 |
| `pcwb/samples.py` / `pcwb/demo.py` | 确定性样本生成与演示入口 |

设计细节与实测数据见 `docs/ARCHITECTURE.md` 与
`artifacts/acceptance/VERIFICATION.md`。

## 错误码

| 代码 | 触发场景 |
| --- | --- |
| `E_POINTCLOUD_INVALID` | magic/CRC/长度错误、坐标越界、形状不合法 |
| `E_COMMIT_FAILED` | 暂存或清单写盘失败、版本文件哈希与清单不符 |
| `E_SELECTION_STALE` | 选区绑定的云哈希与当前版本不一致或索引越界 |
| `E_UNKNOWN_VERSION` | 请求的版本号不存在 |
| `E_CANCELLED` | 编辑任务在协作检查点被取消 |
