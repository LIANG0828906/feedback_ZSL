# Deterministic Tactical Pick Core

一个 C++17、无 GUI 依赖的回合制战术拾取参考实现。它覆盖单位/掩体/地面拾取、框选、草拟区域校验、区域版本化快照、并发发布、异常注入和可逐字节复放的轨迹。

核心验收不依赖 OpenGL 上下文；渲染层可通过相机参数直接调用同一套拾取 API。GLM 固定为 `1.0.1`，仓库内已本地化，CMake 也保留了按 SHA-256 固定版本的下载回退路径。

## 快速开始

Linux:

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
cmake --build build --target test

./build/battle_cli generate --out out/samples --seed 20261121 --cases 3
./build/battle_cli run \
  --scene out/samples/case-000/scene.bin \
  --ops out/samples/case-000/ops.txt \
  --trace out/run/case-000/trace.log \
  --snapshots out/run/case-000/snapshots
./build/battle_cli replay \
  --trace out/run/case-000/trace.log \
  --snapshots out/run/case-000/snapshots
```

Windows:

```bat
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
ctest --test-dir build -C Release --output-on-failure

build\Release\battle_cli.exe generate --out out\samples --seed 20261121 --cases 3
```

一键验证：Linux/macOS 使用 `scripts/validate.sh`，Windows 使用 `scripts\validate.bat`。

## 能力

- 严格二进制场景加载：3D 网格可通行标记、AABB/胶囊单位、掩体三角形、连续实体索引。
- 确定性 BVH：单位与掩体分别构建，查询始终选择最近命中并带稳定平局规则。
- 相机反投影：屏幕坐标到归一化射线，拒绝退化相机、越界坐标、非有限值和零长度射线。
- 不可变区域快照：版本、规范内容哈希、单位/掩体 BVH 与加速结构哈希一起发布。
- 并发隔离：编辑线程在锁外构建完整快照后整体替换；查询只读取已发布快照。
- 草拟校验：自交/越界/非地面顶点直接拒绝；合法多边形逐网格检查可通行性、单位占地和掩体穿模。
- 轨迹证据：浮点以 IEEE-754 位模式记录，SHA-256 哈希链覆盖每条记录，不含时间戳。
- 版本回放：按轨迹版本和内容哈希加载 `.region` 快照并逐位断言命中结果。
- 异常注入门：五类故障均在公共输入入口构造并返回业务码与注入点。
- 确定性样本：固定种子生成三种规模、多个相机位姿、操作序列、基线轨迹和版本快照。

## 业务码

| 业务码 | 值 | 含义 |
| --- | ---: | --- |
| `E_SCENE_INVALID` | 1001 | 场景/实体边界、索引或可通行标记非法 |
| `E_RAY_INVALID` | 1002 | 相机、屏幕坐标或射线非法 |
| `E_REGION_STALE` | 1003 | 期望版本或内容哈希与快照不一致 |
| `E_REGION_INVALID` | 1004 | 草拟多边形自交、越界、非平面或退化 |
| `E_SNAPSHOT_STALE` | 1005 | 句柄已释放、快照损坏或加速结构损坏 |
| `E_TRACE_INVALID` | 1006 | 轨迹字段缺失、时间戳字段、哈希断链或复放不一致 |
| `E_PLACEMENT_REJECTED` | 1007 | 保留的放置拒绝码；规则拒绝同时以 `legal=false` 返回原因 |

## 目录

- `include/battle/`：场景、区域、BVH、相机、查询、放置、轨迹、注入和样本 API。
- `src/`：核心实现与 `battle_cli`。
- `tests/test_main.cpp`：白盒、端到端、并发、异常与逐字节测试。
- `tools/gl33_probe.cpp`：可选 Linux EGL surfaceless OpenGL 3.3 探测。
- `third_party/glm/`：固定版本 GLM 1.0.1。
- `docs/DESIGN.md`：数据结构、算法与一致性设计。
- `docs/VALIDATION.md`：实际工具链、命令和结果。
- `docs/SETUP.md`：Linux/Windows 环境与 OpenGL 检查说明。

## 确定性约定

1. 场景实体按 ID 排序，区域哈希使用固定小端规范序列化。
2. 所有查询一次只绑定一个 `SnapshotHandle`；发布不会原地修改旧快照。
3. 浮点比较不使用时间或哈希遍历顺序作为平局依据。
4. 轨迹记录屏幕、相机、射线、命中点、法向和距离的原始位模式。
5. 轨迹链首哈希为 64 个零，后续 `prev_hash` 必须等于上一条 `record_hash`。
6. 相同构建、种子、相机和操作序列产生逐字节相同的场景、快照与轨迹。
