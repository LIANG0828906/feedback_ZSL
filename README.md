# 本地离线反向成像工作站

面向本地小场景的确定性反向成像工具。场景由球体、平面、针孔相机和方向光组成；正向管线使用解析求交、确定性面积光采样软阴影和 Lambert 光照，反向传播为逐像素反向模式自动微分。参数固定分为 `camera`、`geometry`、`material`、`light` 四组。

项目不使用 Docker，不访问网络资源。样本、目标图、检查点和日志全部由本地确定性随机源与本地文件生成。

## 目录

```text
inverse_imaging/
  scene.py       场景 JSON 校验、目标图与场景标识
  params.py      四组参数、版本号和内容哈希
  autodiff.py    标量反向模式自动微分
  renderer.py    解析求交、软阴影、Lambert、分块梯度
  reduction.py   并行分块计算与固定坐标序归约
  graph.py       参数依赖图、阶段缓存、陈旧缓存否证
  scheduler.py   共享资源池、稳定调度、预算降级
  checkpoint.py  原子检查点、哈希链、轨迹恢复
  engine.py      组调度拟合、发布快照、协作式取消
  gui.py         PyQt5 桌面界面
  samples.py     确定性样本矩阵生成器
  cli.py         命令行入口
tests/           29 项自动化测试
docs/            实际验收记录
```

## 安装与启动

Windows 桌面工作站建议使用 Python 3.10 或 3.11：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -e ".[gui,test]"
python -m inverse_imaging.cli gui --scene path\to\scene.json
```

Linux/macOS 可使用同样的 Python 包入口：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e ".[gui,test]"
python -m inverse_imaging.cli gui --scene samples/medium_24x18/scene.json
```

核心数值、CLI 和测试只依赖 NumPy；PyQt5 仅在启动 `gui` 时导入。若 PyQt5 缺失，CLI 会返回明确错误而不会影响拟合和校验。

## 场景 JSON

最小示例：

```json
{
  "resolution": {"width": 24, "height": 18},
  "camera": {
    "position": [0.0, 0.0, 5.0],
    "rotation": [0.0, 0.0, 0.0],
    "intrinsics": {"fx": 72.0, "fy": 72.0, "cx": 12.0, "cy": 9.0}
  },
  "geometry": {
    "spheres": [
      {"center": [0.0, 0.0, 0.0], "radius": 1.0}
    ],
    "plane": {
      "point": [0.0, 0.0, -2.4],
      "normal": [0.0, 0.0, 1.0]
    }
  },
  "material": {
    "sphere_albedo": [[0.8, 0.4, 0.2]],
    "plane_albedo": [0.2, 0.5, 0.3],
    "ambient": 0.05
  },
  "light": {
    "direction": [0.2, 0.5, 0.8],
    "intensity": 1.2
  },
  "settings": {
    "softness": 0.075,
    "shadow_samples": 5,
    "plane_size": 30.0
  },
  "target": "target.npy"
}
```

约定：

- `target` 是相对场景 JSON 的 `.npy` 路径，形状必须为 `(height, width, 3)`；也接受内联三维数组。
- `rotation` 为弧度，旋转矩阵为 `Rz(yaw) @ Ry(pitch) @ Rx(roll)`，相机局部观察方向为 `(x, y, -1)`。
- 球体的 `sphere_albedo` 数量必须与 `geometry.spheres` 一致。
- `light.direction` 是表面指向光源的向量，渲染前归一化。
- 也支持 `camera.rotation_matrix` 和 `camera.extrinsics.world_to_camera`。后者为 4x4，校验行列式并自动换算相机位置与欧拉角。
- 任何几何退化、非有限数、不可逆矩阵、光照方向为零或目标图尺寸不符都会抛出 `E_SCENE_INVALID`，并在 `field` 中给出字段路径。

## 样本生成

```bash
python -m inverse_imaging.cli generate \
  --out runs/sample_matrix --seed 7 --matrix
```

生成低、中、高耦合三组本地场景，包含不同分辨率、球体数、软阴影宽度、参数分组文件、目标图和 `manifest.json`。相同种子与相同输出目录重复生成，场景 JSON、目标图和分组文件逐字节一致。

单样本：

```bash
python -m inverse_imaging.cli generate \
  --out runs/demo --seed 11 --width 24 --height 18 --spheres 3
```

## 命令行

### 拟合

```bash
python -m inverse_imaging.cli fit \
  --scene runs/demo/scene.json \
  --steps 8 \
  --groups camera,geometry,material,light \
  --chunk-size 8 \
  --parallel 2 \
  --budget-mb 256 \
  --task-quota-mb 48 \
  --checkpoint-dir runs/demo/checkpoint
```

`--groups` 是组调度序列，按步循环。每步只更新当前参数组，但输出四组梯度。

### 验收

```bash
python -m inverse_imaging.cli verify \
  --scene runs/demo/scene.json \
  --parallels 1,2,4 \
  --chunks 2,4,8 \
  --steps 4 \
  --report runs/demo/verification.json
```

覆盖：

- 多并行度、多分块尺寸的梯度归约；
- 增量复用与全量重算一致性；
- 上游版本改变但依赖图未标记失效时的否证；
- 缓存内容哈希被破坏时的否证；
- 不中断与强制终止恢复的轨迹/检查点字节比较；
- 资源池稳定序列、等待与预算拒绝路径。

### 一键演示

```bash
python -m inverse_imaging.cli demo --workdir runs/acceptance --seed 7
```

输出 `runs/acceptance/demo_report.json`。

### 进程级恢复

在提交指定步后直接退出进程：

```bash
python -m inverse_imaging.cli fit ... --hard-exit-after-step 1
python -m inverse_imaging.cli fit ...   # 使用同一 checkpoint-dir 恢复
```

第一次以退出码 `17` 终止，第二次仅依据磁盘检查点继续。已验证恢复后轨迹和检查点与不中断执行逐字节一致。

## 关键实现

### 正向与反向

- 相机光线、球/平面解析求交、最近命中可见性、固定朝向面积光采样软阴影、Lambert 着色和逐像素 RGB L2 损失均可微。
- 每个像素建立独立标量反向图；一个分块内按像素坐标顺序累积到该分块的四组梯度。块间没有共享可变图状态，因此可在线程池中并行。
- `forward_scene()` 返回损失、光线、交点、法线、可见性、阴影因子和着色图；`compute_gradients()` 返回损失、四组梯度和归约元数据。

### 确定性归约

- 分块坐标固定为 `(x0, y0, x1, y1)`，归约前按 `(y0, x0, y1, x1)` 排序，与完成顺序无关。
- 每个分块记录梯度内容哈希；全程记录 `reduction_order`。
- 归约容差默认 `atol=1e-12`、`rtol=1e-10`。同一分块布局下发现差异时抛出 `E_REDUCE_DIVERGE`，上下文包含首个不一致分块编号和坐标。

### 依赖图与缓存

正向节点为：

```text
camera_rays -> intersections -> visibility -> shadow_factor -> shading -> loss
```

四组参数分别连接到受影响的中间节点。缓存键同时包含：

```text
(参数组版本号, 参数组内容SHA-256)
```

因此“版本变了但调用方忘记标记失效”和“缓存值被篡改但记录哈希未变”都会被拒绝。拒绝后清除/重建缓存并发布全量重算结果；轨迹记录 `stale_dependency:*` 或 `cache_hash_mismatch:*` 原因。否证后的损失和梯度与同分块布局的从零重算逐字节一致。

### 资源池

- `BudgetConfig` 固定全局并行槽位和内存上限，运行期不修改配置。
- 任务按提交序号稳定准入；预算不足进入等待队列。
- 单任务请求超过其配额时按声明的分块降级序列缩批并重新估算；无可用尺寸时返回 `E_BUDGET_EXCEEDED`，不改变其他任务状态。
- 调度事件以 `submitted / waiting / degraded / admitted / completed / failed / rejected` 记录，事件内容哈希写入 `scheduler.jsonl`。

### 检查点与恢复

每次提交写入：

- 四组参数和参数版本；
- 累计损失、本步损失、四组梯度范数；
- 依赖图版本、分块归约哈希、分块哈希列表和归约顺序；
- 优化器状态与完整确定性轨迹历史；
- `prev_hash` 和 `self_hash` 哈希链。

检查点先写同目录临时文件，`fsync` 后 `os.replace` 原子重命名。恢复扫描全部 `checkpoint-XXXXXXXX.json`，校验格式版本、场景标识、步号连续性、内容哈希和链哈希；断链或篡改返回 `E_RECOVERY_INVALID`。

恢复只使用磁盘检查点，从最后提交步的下一步继续。恢复时用当前参数重建前向缓存，因此下一步的复用/重算节点计数与不中断运行一致。

### 并发边界与界面

- 进程内同一 `scene_id` 只允许一个拟合任务，重复启动返回 `E_BUSY`。
- 训练线程每个提交边界发布不可变快照；查询线程只读取已发布快照。
- 查询旧句柄返回 `E_STALE_HANDLE`。
- PyQt5 界面展示四组参数树、损失曲线、受影响/复用/重算节点数、等待队列、降级原因和轨迹状态；支持启动、协作式取消和按组编辑。

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_SCENE_INVALID` | 场景字段、几何、矩阵、光照或目标图无效，带字段路径 |
| `E_REDUCE_DIVERGE` | 分块或归约梯度超出声明容差 |
| `E_BUDGET_EXCEEDED` | 任务缩批后仍超过配额或全局预算 |
| `E_RECOVERY_INVALID` | 检查点格式、版本、步号、内容哈希或哈希链损坏 |
| `E_BUSY` | 同一场景已有拟合任务或会话重复启动 |
| `E_STALE_HANDLE` | 查询已废弃的发布快照 |
| `E_IO` | 本地文件读取或原子写入失败 |

## 验证

当前环境实际运行结果见 `docs/VALIDATION.md`：

- `29 passed`；
- 9 组并行度/分块组合最大绝对梯度差 `5.42e-19`；
- 增量结果与全量重算等价；
- 两类陈旧缓存否证均触发全量降级；
- 正常/中断恢复轨迹和检查点逐字节一致；
- 硬终止退出码 `17`，恢复成功；
- PyQt5 在 `QT_QPA_PLATFORM=offscreen` 下完成窗口构造和退出。

Windows 上的真实可见窗口需在目标桌面会话执行上面的 `gui` 命令；当前验证环境为 Linux 工作站，无物理 Windows 显示会话。

