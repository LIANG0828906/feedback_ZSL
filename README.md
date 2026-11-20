# 光谱材质色彩管理编辑器

本地确定性实现的影视/游戏材质库原型：导入光谱反射率与既有 sRGB、Display-P3 材质，执行统一色彩管理链路，记录可回溯版本，支持增量重算、原子发布、并发读取隔离、强制终止恢复和 Qt 交互界面。

## 快速开始

要求 Python 3.11+。在 Windows、Linux 或 macOS 本地图形环境中执行：

```bash
python -m venv .venv
.venv/bin/pip install -e ".[test]"

# 生成样本、基线哈希和初始快照
.venv/bin/python -m spectral_editor generate --seed 1907 --out artifacts/samples

# 运行七代版本演进演示
.venv/bin/python -m spectral_editor demo --seed 1907 --out artifacts/demo

# 两次独立演示逐字节比较
.venv/bin/python -m spectral_editor verify --seed 1907 --out artifacts/verify

# 启动编辑器
.venv/bin/python -m spectral_editor gui --snapshot artifacts/demo/state/snapshot.json

# 无显示环境截图或 CI 冒烟
QT_QPA_PLATFORM=offscreen .venv/bin/python -m spectral_editor gui \
  --snapshot artifacts/demo/state/snapshot.json \
  --screenshot artifacts/gui-offscreen.png --exit-after-ms 800
```

Windows 下将 `.venv/bin/` 替换为 `.venv\Scripts\`。整个项目不使用 Docker，样本、观察者配色函数和光源光谱均由脚本本地生成。

## 功能实现

- `spectral_editor/materials.py`：校验材质 JSON、波长严格递增、采样间隔、反射率 `[0,1]`、目标色彩空间和显示材质编码值；统一网格线性重采样，记录原始采样点数、原始网格、真值来源和源哈希。
- `spectral_editor/color.py`：执行光源加权归一化、观察者积分、白点适应（Bradford、Von Kries、none）、XYZ 到 sRGB/Display-P3 矩阵变换和分段传递函数编码；中间量保留光源哈希、积分归一化、源/目标白点、适应矩阵、线性与编码 RGB。
- `spectral_editor/versioning.py`：版本内容哈希、逐版本哈希链、发布哈希和版本差异。数值统一量化到 12 位小数后再哈希，保证内存态和落盘恢复态一致。
- `spectral_editor/library.py`：计算受影响材质集合，只新增受影响版本；其他版本原对象、内容哈希和链哈希原样复用。发布使用不可变快照与短临界区指针交换。
- `spectral_editor/persistence.py`：JSON 临时文件 + `fsync` + 原子重命名；校验快照字段、版本连续、父子版本、内容哈希、逐版本链哈希、发布链单调性、UI 状态和独立恢复锚点。
- `spectral_editor/runtime.py`：单库单后台重算任务；重复提交返回 `E_BUSY`，取消事件在材质边界检查。
- `spectral_editor/gui.py`：sRGB/Display-P3 并排色块与数值、版本时间轴、依赖因子高亮、版本差异、回退、白点/观察者/光源 CCT/色适应编辑和自定义光源 JSON 导入。完成后才刷新为新发布快照。

## 一致性模型

1. 每次提交先写 `state/pending.json`，其中记录基线代、基线发布哈希和完整请求。
2. 后台线程只构建候选版本集，不修改当前快照。
3. 候选完成后先原子写 `state/snapshot.json`，再写 `state/recovery_anchor.json`，最后在短暂持有指针锁时整体替换发布指针。
4. 发布前读线程始终拿到旧快照的完整不可变句柄；已发出的旧句柄在释放前继续返回同一发布代的数据。
5. 句柄释放后再次读取返回 `E_SNAPSHOT_STALE`。发布指针交换不执行重计算和磁盘 I/O，因此读锁等待为微秒级。
6. 若进程在 pending 写入后死亡，恢复得到最后已发布代和 pending 请求；续算只处理本次受影响材质。若快照已提交但 pending 尚未删除，恢复流程能依据基线发布哈希识别该变更已提交。
7. 恢复锚点记录最高提交代和发布哈希，可发现“整份快照被替换成更旧代”的回退。

## 业务码

| 业务码 | 场景 |
| --- | --- |
| `E_MATERIAL_INVALID` | 材质字段缺失、路径非法、波长乱序、间隔不符、值越界 |
| `E_PIPELINE_UNINIT` | 色彩链路未初始化、观察者/光源/网格不可用 |
| `E_VERSION_CONFLICT` | 版本不存在、head 哈希变化、并发发布冲突 |
| `E_RECOVERY_INVALID` | 快照字段缺失、内容/链哈希错误、版本回退、锚点冲突 |
| `E_BUSY` | 同一材质库已有后台重算任务 |
| `E_SNAPSHOT_STALE` | 句柄已释放或非本库发放 |
| `E_CANCELLED` | 协作式取消检查点触发 |

## 数据与日志

- `artifacts/samples/materials.json`：多波长网格、多曲率光谱及 sRGB/P3 显示材质。
- `artifacts/samples/observer_2deg.json`、`observer_10deg.json`：确定性解析式 CIE 观察者近似。
- `artifacts/samples/illuminant_d65.json`：确定性 Planck 光谱。
- `artifacts/demo/trace.jsonl`：每次发布的版本号、依赖因子、受影响集合、复用/重算计数、最大通道偏差、编码值和内容哈希。
- `artifacts/demo/state/snapshot.json`：最终已发布版本集、UI 状态和发布链。
- `artifacts/gui-offscreen.png`：离屏 Qt 验收截图。

哈希真值只来自规范化内容 JSON，不使用时间戳、文件大小或文件 mtime。

## 实际验证

```text
$ .venv/bin/python -m pytest -q
.......................................                                  [100%]
39 passed
```

七代演示统计：

| 操作 | 发布代 | 重算 | 复用 |
| --- | ---: | ---: | ---: |
| 初始发布 | 1 | 8 | 0 |
| 光源 5000 K | 2 | 6 | 2 |
| 白点 D50 | 3 | 8 | 0 |
| Von Kries | 4 | 8 | 0 |
| 观察者 10° | 5 | 6 | 2 |
| 单材质编辑 | 6 | 1 | 7 |
| 回退到 v1 | 7 | 1 | 7 |

最终恢复结果：`matches_published=true`、`pending_job=false`、UI 选择与版本恢复一致。

确定性校验：9 个文件（包括快照、轨迹、摘要、恢复锚点）在相同种子与操作序列的两次运行中逐字节一致。

最终产物 SHA-256：

```text
snapshot.json       8d972ceb19a6041fb4432c7279be98784883039a04be54032505ca9c1082a40c
trace.jsonl         98696374bd812aacc15a01877b0c43217181756768cea84d7c4ef6d26bb25b44
summary.json        493d47a8e59dfe1fcd8de2ea4f727de76d9003c78c76ceb9dcf0f1407b4c7d82
recovery_anchor     3e71f51c900a2678868530850ebff68964deb71240b9b6bc3a8673bd306d15ad
gui-offscreen.png   e93a2183b0cfdeb8bac2922c32cd5c97cf445b0c69d958e619e988f10ed9b81c
```

强制终止测试由 `tests/test_persistence.py` 在独立子进程执行：写入 pending journal 后以 `os._exit(37)` 退出；父进程恢复到最后发布代，续算得到 6 个光谱材质重算、2 个显示材质复用，已发布版本哈希未改写。

## 说明

观察者函数采用确定性多叶高斯解析近似，适合本任务的一致性验证与本地样本，不作为计量级 CIE 数据集替代。生产环境可替换为经授权的标准 CMF 表，链路和持久化接口无需改变。

当前执行环境为 Linux，Qt 验收采用 `offscreen` 平台完成；本机没有 Windows 图形会话，因此未声称已实机启动 Windows Qt。实现只使用 PySide6 与跨平台文件 API，并对 Windows 不允许目录句柄 `fsync` 的情况做了兼容处理。
