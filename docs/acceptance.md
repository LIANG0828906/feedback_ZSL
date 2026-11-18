# 验收记录与复算说明

## 环境

- 日期：2026-11-18（Asia/Shanghai）
- 系统：Linux 容器外本地工作区（非 Docker）
- 编译器：GCC 15.2.0
- 构建系统：CMake 3.30.5
- 渲染后端：纯 CPU C++17，无 GPU、无外部图像库
- 实际工作区没有 `ctest` 可执行文件，因此通过同一 CTest 注册的 Python 验收脚本直接运行。

Windows 10/11、Visual Studio 2022 的构建命令已在 `README.md` 中给出；当前执行环境不是
Windows，因此没有记录 MSVC 运行结果。Windows 分支包含 `_commit` + `MoveFileExW` 原子
替换和 `SetConsoleCtrlHandler` 中断处理。

## 确定性样本

通过以下命令构造，不读取任何外部资源：

```bash
./build/softracer generate --out scene.json
./build/softracer render --scene scene.json --out out
```

场景参数：

- 图像：480×320，RGBA8，2×2 像素子采样。
- 分块：80×80，共 6×4 = 24 块。
- 粒子：22 层，每层 5 个四边形，共 110 个。
- 光源：2 个暖色点光源。
- 相机：16 个严格递增时间戳的环绕样本，默认第 0 帧。
- 阴影采样密度：9；半影系数：0.055。

## 结果摘要

```text
scene_sha256         = dff1f8508af4abc1100cb0d9d4d127a93d37a71c94bd43435069c076bce16350
render.rgba8 sha256 = 8ea498ed46a6d139b0e97c1a790da658c4b4454c7ac9f04ee9829d61d00cf40a
render.png sha256   = ab4a7a661281a9d2866400131afae0d5ea67fb5d69755f094b4b2feeb14f9c50
report.json sha256  = f97f4dd3d9e1d2c61d6d8fa012edb96bc426d4579e2c2ccbe6a22e761c117d15
visible_fragments   = 40118
average_transmittance = 0.83097778722626
active_shadow_landings = 81 / 220
```

多光源阴影探针示例（帧 0）：

```text
probe auto-shadow-1-1033
  linear_luminance = 0.088785
  light 1 share = 0.530623, visibility = 0.268908
  light 2 share = 0.469377, visibility = 0.576557
  share_sum = 1
```

半影控制样本随层到地面距离和光源到层距离均为严格递增；实际边缘 profile 从覆盖率 1
平滑过渡到 0，并同步记录 9 个亮度采样点。

## 恢复与逐字节比较

强制终止模拟：

```bash
./build/softracer render --scene scene.json --out crash \
  --crash-after-new-commits 4
# 退出码 86，snapshot.bin 保留完整提交状态
./build/softracer render --scene scene.json --out crash
./build/softracer compare --reference full --candidate crash \
  --out crash/difference_report.json
```

比较结果：`images match byte-for-byte`，退出码 0。

快照损坏测试在 `snapshot.bin` 中部翻转一个字节，重启日志包含 `discarded snapshot`，
随后从第 0 块重算，结果仍与完整渲染逐字节一致。

## 视觉检查

参考图位于实际验收输出目录：

- `build/acceptance/full/render.png`
- `build/acceptance/full/smoke_premultiplied.png`
- `build/acceptance/full/ownership.png`
- `build/acceptance/full/report.json`

参考图可见暖色地面、烟柱投影形成的地面明暗区域，以及从遮挡核心向外的连续半影。
