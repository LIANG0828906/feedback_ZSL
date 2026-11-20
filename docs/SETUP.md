# Setup

## Linux

基础依赖：

```bash
sudo apt update
sudo apt install -y build-essential cmake
```

CMake 最低 3.20。参考验证环境使用 GCC 15.2 与 CMake 3.30。

核心是 headless 的，不要求窗口系统。若部署层需要 OpenGL 3.3，额外安装运行时工具：

```bash
sudo apt install -y libgl1-mesa-dev mesa-utils
glxinfo -B
```

在 `OpenGL core profile version string` 中确认版本不低于 3.3。无显示环境也可用仓库自带的 EGL surfaceless 探测：

```bash
cmake -S . -B build -DBATTLE_BUILD_GL_PROBE=ON
cmake --build build --target gl33_probe
./build/gl33_probe
```

当前验收容器没有 OpenGL 开发头和显示服务，默认 CMake 会输出：

```text
OpenGL libraries not found; building the headless deterministic core
```

可选探测仍成功创建了 OpenGL 3.3 Core 上下文，实际报告 4.5 Core Profile。默认拾取内核、样本和回放验收不依赖该上下文。渲染宿主只需把相机矩阵/位姿传入 `battle::Camera`。

## Windows

推荐 Visual Studio 2022 Build Tools + CMake：

```powershell
winget install Kitware.CMake
winget install Microsoft.VisualStudio.2022.BuildTools
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
ctest --test-dir build -C Release --output-on-failure
```

MinGW-w64 也可使用 Ninja 或 MinGW Makefiles：

```bat
cmake -S . -B build-mingw -G Ninja -DCMAKE_BUILD_TYPE=Release
cmake --build build-mingw
ctest --test-dir build-mingw --output-on-failure
```

Windows 命令行可直接运行：

```bat
build\Release\battle_cli.exe generate --out out\samples --seed 20261121 --cases 3
build\Release\battle_cli.exe run --scene out\samples\case-000\scene.bin --ops out\samples\case-000\ops.txt --trace out\run\case-000\trace.log --snapshots out\run\case-000\snapshots
build\Release\battle_cli.exe replay --trace out\run\case-000\trace.log --snapshots out\run\case-000\snapshots
```

确保 `out\samples`、`out\run` 和测试工作目录对当前用户可写。

Windows 下 OpenGL 3.3 可用性应使用显卡厂商工具或宿主渲染程序检查；当前可选 `gl33_probe` 仅实现 Linux EGL surfaceless 路径，未在 Windows 构建。

## GLM

仓库内 `third_party/glm` 为 GLM `1.0.1`。若删除该目录，CMake 会通过 `FetchContent` 下载固定归档：

```text
URL: https://github.com/g-truc/glm/archive/refs/tags/1.0.1.tar.gz
SHA256: 9f3174561fd26904b23f0db5e560971cbf9b3cbda0b280f04d5c379d03bf234c
```

正常构建和运行不需要网络、数据库或容器。

## Sanitizer build

```bash
cmake -S . -B build-sanitize -DCMAKE_BUILD_TYPE=Debug -DBATTLE_ENABLE_SANITIZERS=ON
cmake --build build-sanitize -j
ASAN_OPTIONS=detect_leaks=1 ./build-sanitize/battle_tests ./test-work-sanitize
```

