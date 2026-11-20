# 赛事回放视频帧取证引擎（vforensics）

面向帧序列内容完整性校验与证据链路重建的 C++17 / Qt6 / OpenCV 4 工程。
完整设计、格式规范与验证记录见 `docs/TECHNICAL.md`。

## 快速开始

```bash
source scripts/env.sh
cmake -S . -B build -DCMAKE_PREFIX_PATH="$QT6_PREFIX" -DCMAKE_BUILD_TYPE=Release
cmake --build build -j8
(cd build && ctest --output-on-failure)
./scripts/demo.sh
```

## 目录结构

- `include/vforensics/` 公共头：类型、帧格式、清单、图、引擎、快照、worker、报告
- `src/` 核心库 + Qt 时间轴界面
- `tools/` `vf_cli`（verify/resume/report/busy-test）与 `vf_generate`（确定性样本）
- `tests/` 5 个 Qt Test 套件
- `config/sample_profiles.json` 本地样本参数（种子、分辨率、失配位置、清单版本）
- `scripts/env.sh` 本地 Qt/OpenCV 环境；`scripts/demo.sh` 脚本化验收演示
- `artifacts/` 演示产物：样本、快照、诊断报告、轨迹日志与运行日志

## 二进制与清单输入

- `.vfseq`：自定义帧序列（魔数 `VFSE`，帧头表含时间戳与逐帧 SHA-256，后接原始像素）
- `.vref`：与 `.vfseq` 同构的原始像素参考，用于首个失配字节定位
- `.manifest.json`：依赖边（child→parents）与分段水印锚点，支持 v1/v2 语义比对
