# Church Floor Mosaic Offline Validator

一个无外部素材、无网络依赖的确定性离线验证器，用于检查教堂地坪玻璃嵌片在多个点光源下的：

- 多层透射衰减与预乘 Alpha 合成；
- 边缘折射率驱动的眩光与反射/透射贡献；
- 内层边缘向相邻外层嵌片及地坪基底的焦散投影；
- 软半影、厚度/层距趋势、逐字节可复现的图像与投影落点；
- 分块快照、优雅中断续算、强制终止恢复和结构化差异报告。

实现使用 C++20 的确定性 CPU 离屏光栅/光线求交核心，不依赖 GPU 驱动或浮点并行归约顺序。因此本地 Mesa/OpenGL 4.6 环境可用于其他工具链检查，但验证器本身**不需要创建 OpenGL 上下文**，避免驱动差异破坏逐字节复现。

## 快速开始

### Linux

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r python/requirements.txt

cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
./build/mosaic_core_tests

python python/generate_scene.py
./build/mosaic-validator validate --scene samples/church_floor.json
./build/mosaic-validator render \
  --scene samples/church_floor.json \
  --out artifacts/acceptance/full
./build/mosaic-validator verify \
  --a artifacts/acceptance/full \
  --b artifacts/acceptance/repeat \
  --report artifacts/acceptance/full_vs_repeat.json
```

### Visual Studio 2022

安装 `Desktop development with C++`、C++20 和 CMake 3.21+，然后：

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
.\build\Release\mosaic_core_tests.exe
.\build\Release\mosaic-validator.exe validate --scene samples\church_floor.json
```

Python 3.10+ 与 NumPy 用于样本生成和验收脚本。NumPy 不是 C++ 运行时依赖。

## 一键验收

```bash
python python/run_acceptance.py
```

脚本会构建并运行核心单测，然后实际验证：

- 干净运行两次的 RGBA、归属图、焦散灰度与纹理表逐字节一致；
- 嵌片/灯光/三角形遍历顺序打乱后结果仍一致；
- 优雅中断在第 5 块后退出码 `0`，重启只复用已提交块；
- `SIGKILL` 后从持久化快照恢复，最终结果与完整运行逐字节一致；
- 块文件损坏、清单 JSON 损坏、场景摘要不一致时全部缓存丢弃并从零重算；
- 厚度增加导致焦散投影落点偏移；
- 反转声明合成顺序会改变图像，且通道不溢出；
- 业务校验错误均返回字段路径、业务码和非零退出码。

结果写入 `artifacts/acceptance/acceptance_result.json`。完整样本输出位于 `artifacts/acceptance/full/`。

## 实际验证结果

本机（GCC 15.2、CMake 3.30、Python 3.14、NumPy 2.5.5）实测：

- 验收：`30/30` 通过；
- 样本：`320x240`，`64x48` 分块，共 `25` 块，26 片嵌片，3 层，3 个点光源；
- `image.rgba` SHA-256：`c22730f5d18e9524e70bf546a56712c42046e4a668bd1d4a83436fefdc5e5fc8`；
- `ownership.u64` SHA-256：`f18948f9784bd5fffb263c561f62a864fe1467cdb8617269f42c42d8c3ac7cad`；
- `caustic_gray.u16` SHA-256：`1509427ded2be06615a0c7ef71235c4ff7ce6c43d5f60b790b97793d38bb441f`；
- 焦散记录 `4661` 条，连通亮纹 `10` 条；
- 各层可见片元数：`16716 / 16620 / 15591`；
- 半影宽度趋势：`2.095130 px < 3.208172 px`（厚度与层距同时增加）；
- 近白过曝像素比例：`0`；同层相邻嵌片最小平均 RGB RMS 差：`2.539/255`；
- 优雅中断与 `SIGKILL` 均在 5 个已提交块后恢复，恢复后最终图像逐字节一致；
- 损坏块恢复原因：`snapshot-chunk-file-digest`；损坏清单：`snapshot-parse-error`；场景不一致：`scene-digest-mismatch`，三种情况均 `reusedChunks=0` 后重算。

## 命令

```text
mosaic-validator validate --scene <scene.json>
mosaic-validator render --scene <scene.json> --out <dir>
  [--cache <dir>] [--interrupt-after-chunks <n>]
  [--chunk-delay-ms <n>] [--force-restart]
mosaic-validator verify --a <run-a> --b <run-b> --report <difference.json>
```

退出码：

- `0`：成功，或可处理中断已提交当前块并写出恢复报告；
- `2`：命令行错误；
- `20`：场景/JSON 校验错误；
- `30`：I/O、快照或输出错误；
- `40`：验证发现差异；
- `41`：验证输入缺失或不可读。

渲染产物：

```text
image.rgba                  预乘 Alpha RGBA8，行主序
image.png                   同一像素数据的无外部依赖 PNG
ownership.u64               每像素 uint64 归属键，小端
ownership_legend.json       归属键到层号/嵌片号映射
caustic_gray.u16            焦散灰度，小端 uint16
caustic_gray.png            焦散灰度可视化
caustics.bin                固定布局焦散投影记录
caustics.json               可读投影落点/半影/亮度记录
texture_table.bin           逐层/逐片确定性气泡与流纹采样表
report.json                 稳定字段顺序统计报告
recovery.json               本次恢复/中断结果
snapshot/snapshot.json      原子提交的分块快照清单
snapshot/chunks/*.bin       已提交块载荷
```

报告不包含时间戳或绝对路径。

## 文档

- `docs/SCENE_FORMAT.md`：JSON 模式、字段范围、业务错误码和字段路径；
- `docs/RENDERING_AND_RECOVERY.md`：确定性纹理、光路/焦散模型、合成、快照协议和差异格式。

## 边界说明

该程序是美术意图验证器，不是光谱级物理模拟器。玻璃内部气泡/流纹由场景种子和嵌片键直接哈希生成；焦散采用可复算的边缘采样、折射切向偏移和高斯半影核。所有语义参数、阈值和业务码均在文档与代码中固定，不使用系统随机数。
