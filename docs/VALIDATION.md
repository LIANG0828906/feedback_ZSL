# 本机验证记录

环境：

```text
OS: Ubuntu 26.04 (local, no container)
Compiler: g++ 15.2.0
CMake: 3.30.5
CPU: 24 logical cores
```

构建：

```bash
cmake -S . -B build -DMESHREPAIR_BUILD_TESTS=ON -DMESHREPAIR_BUILD_UI=ON
cmake --build build -j8
ctest --test-dir build --output-on-failure
```

实际结果：

```text
100% tests passed, 0 tests failed out of 1
checks=206 failures=0
ASan+UBSan build: 100% tests passed, 0 failures
```

覆盖项：

- SHA-256 与标准测试向量一致。
- 二进制 mesh 往返、坏索引、NaN 坐标、面片数/长度不符定位。
- 配置缺失、零并行度、单块预算大于全局预算。
- 分块表/边界映射在并行度 1 与 4 下哈希一致。
- 跨块孔洞为 4 顶点，填补 2 面，涉及 3 个分块。
- 焊接移除 1 个重复顶点；非流形边拆分 1 条、3 组、复制 2 个稳定顶点。
- 孤立碎片删除 1 个面。
- 拼接后 17 顶点、17 面，边界边校验与重复填补拦截通过。
- 单块预算拒绝包含块号和预估字节数，且未启动该块。
- 全局预算限制会生成确定性的 `WAIT_QUEUE / reason=GLOBAL_BUDGET` 轨迹，并最终完成。
- 同网格第二会话返回 `E_BUSY`；损坏 chunk、链缺口和损坏 plan 分别定位到块 1、1、0。
- 取消发生在提交边界，未提交块不落盘。
- 快照提交计数单调，最后一个快照包含全部已提交块。
- `--stop-after 2` 强制 `_exit(137)` 后续修，`repaired.mesh`、`trace.log`、`stitch.sha256` 与未中断执行逐字节相同。

样本统计（seed=42）：

```text
original_vertices=17
vertices_after_weld=16
welded_vertex_removals=1
final_vertices=17
original_faces=16
final_faces=17
filled_faces=2
split_edges=1
split_groups=3
split_vertex_copies=2
debris_faces_removed=1
stitch_valid=true
normal_mean_deg≈1.37e-07
normal_max_deg≈1.21e-06
```

强杀续修对比：

```text
MESH_CMP=0
TRACE_CMP=0
STITCH_HASH_CMP=0
```

OpenGL/GLFW 验证：

- `src/ui.cpp` 使用 GLFW 3.4 + Mesa GL 头文件成功编译和链接。
- 在本地 X11 显示上以 `--frames 45` 运行，后台修复完成且进程正常退出 0。
- 运行环境存在软件渲染的 EGL/Zink 警告，但窗口、快照更新和关闭流程均完成；核心修复不依赖图形栈。

如果系统未安装 GLFW/OpenGL 开发包，CMake 会构建 `meshrepair_ui` stub；核心 CLI 与全部测试仍可构建和运行。
