# Validation

验证日期：2026-11-21。环境：Linux x86_64，GCC 15.2.0，CMake 3.30.5，24 逻辑核。

## 1. Build

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j 8
```

结果：

```text
[100%] Built target battle_tests
```

编译配置启用 `-Wall -Wextra -Wpedantic -Wconversion -Wshadow`，未产生项目源码警告。

## 2. Unit/integration tests

```bash
./build/battle_tests ./test-work
cmake --build build --target test
```

结果：

```text
all battle tests passed
1/1 Test #1: battle_unit_and_integration ... Passed
100% tests passed, 0 tests failed out of 1
```

覆盖内容：

- 合法场景往返编码，缺失可通行标记、非法记录索引、退化 AABB/胶囊、坏形状标签；
- AABB、胶囊、掩体、地面命中，相机退化、越界屏幕、零长度射线；
- 同版本重复拾取逐位一致；
- 旧句柄在新版本发布后仍返回旧版本结果；
- 期望版本/哈希不匹配返回 `E_REGION_STALE`；
- 释放句柄与损坏快照返回 `E_SNAPSHOT_STALE`；
- 4 读线程与持续发布写线程并发运行，抽样版本回查结果逐位一致；
- 草拟合法、单位冲突、掩体冲突、阻挡单元、自交、越界、非地面顶点；
- 五类异常注入返回独立业务码和注入点；
- 轨迹缺失版本、时间戳字段、内容篡改、哈希断链；
- 按版本快照完整回放；
- 三次样本生成目录逐文件一致；
- 样本重跑与基线逐字节一致。

## 3. Sanitizers

```bash
cmake -S . -B build-sanitize -DCMAKE_BUILD_TYPE=Debug -DBATTLE_ENABLE_SANITIZERS=ON
cmake --build build-sanitize -j 8
ASAN_OPTIONS=detect_leaks=1 ./build-sanitize/battle_tests ./test-work-sanitize
```

结果：

```text
all battle tests passed
```

AddressSanitizer、UndefinedBehaviorSanitizer 与泄漏检查未报告问题。

## 4. Sample generation

```bash
./build/battle_cli generate --out out/final-samples --seed 20261121 --cases 3
```

结果：

```text
generated_cases=3 seed=20261121 manifest_hash=372c4b5533fbf81cfcbb42979915c067e9113e6cc8591bea694d611331bd5d64
```

三个案例分别覆盖 `16x4x12`、`24x4x18`、`40x4x28` 网格，单位数 `6/14/28`，掩体数 `4/8/14`，并含多相机位姿、单位移动、掩体存在性变化、网格可通行状态变化和合法/拒绝草拟。

每个案例生成：

- `scene.bin`；
- `ops.txt`；
- `baseline/trace.log`；
- 版本 `1..4` 的 `.region` 快照。

## 5. Cross-process rerun and replay

对三个案例分别执行 `run` 和 `replay`，并将实际轨迹与基线比较：

| 案例 | 版本 | 记录数 | 最终哈希链 | 轨迹 SHA-256 |
| --- | ---: | ---: | --- | --- |
| `case-000` | 4 | 10 | `d6e3d0222049cc30cf909fa980bba24c4c8e0f6b3a4fae06ebae66938534cd3d` | `4338c56b3bf923179fe7ab461fa882d3dad1eda25177ef7f92519a9b12617b7d` |
| `case-001` | 4 | 10 | `5b7d87226cc2ef824819e4c04d68a0a42577eebe5d2112e76615286103873762` | `2d6325e2a5463e04d1cbd64e81394570aed631a0db4e1a721fcd7c5709b9fd6c` |
| `case-002` | 4 | 10 | `41a15c97feab37046880c78c6726e4bdfdf0ecdeec531905e96fc5c216afc0d7` | `d2ba01373d5bbb4b1142d80d812a24e699de81fe3d758e317e8acc0605cdcbb8` |

三个案例的实际轨迹均通过 `cmp` 与基线逐字节比较。`replay` 分别回放 10 条记录，最终哈希与轨迹一致。

## 6. OpenGL status

本环境没有 OpenGL 开发头、`glxinfo` 或显示服务，但 Mesa 运行时支持 EGL surfaceless。使用可选探测成功创建 3.3 Core 上下文并提升到实际驱动能力：

```text
vendor=Mesa
renderer=llvmpipe (LLVM 21.1.8, 256 bits)
version=4.5 (Core Profile) Mesa 26.0.8-1ubuntu0.3
OPENGL_3_3_OK
```

探测命令：

```bash
cmake -S . -B build -DBATTLE_BUILD_GL_PROBE=ON
cmake --build build --target gl33_probe
./build/gl33_probe
```

Windows 或带显示 Linux 的检查步骤见 `docs/SETUP.md`。本交付的拾取、版本、放置、并发和轨迹验收均不依赖 OpenGL 上下文。

