# Validation Record

验证日期：2026-11-20。

## 环境

- WSL2 Ubuntu 26.04。
- Python 3.14.4（使用与 Python 3.11 兼容的语法和标准库 API）。
- NumPy 2.5.5，作为唯一运行时第三方依赖。
- 非交互式本地运行，无 Docker、GPU、OpenGL 或网络数据访问。

## 自动测试

命令：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
```

结果：14 项测试全部通过，覆盖：

- 高度场/材质往返、损坏负载、越界高程和非法头。
- 四叉树节点/块重建报告、真值来源和覆盖范围极值。
- LOD 同视点确定性、层级分化、预算使用与误差上界。
- 匹配顶点接缝全部闭合，以及 `E_CRACK_UNRESOLVED` 的块/边定位。
- 平台默认法线、归一化、材质增量重算和格距变化全量重算。
- 连续两次渲染缓冲区完全一致，精度切换输出深度差异。
- 快照哈希检测、视点/层级变更识别、原子快照保留。
- `E_BUSY`、协作式取消，以及取消后上次快照字节不变。
- 相同种子样本、基线轨迹和快照跨目录逐字节一致。

## 多地形端到端复现

命令：

```bash
PYTHONPATH=src python3 -c "from terrainviz.demo import run_demo; run_demo('/tmp/terrainviz-a')"
PYTHONPATH=src python3 -c "from terrainviz.demo import run_demo; run_demo('/tmp/terrainviz-b')"
```

两次独立目录运行结果：

- `report_hash` 均为 `e8922ada8e2ce16d205c4593d313a61f2abf351da8808adfd61f73c889def439`。
- 平原、山地、混合三套地形均完成 LOD、接缝修补、法线、材质和离屏渲染。
- 三套地形连续两次 `float64` 渲染的颜色、深度、热力图逐字节一致。
- 共比较 22 个输出文件，跨目录不匹配数为 0。
- 快照 SHA-256：
  - plain: `9951b61033092564ba99b925bbe1eacceec320e3a185863e16b09b4b94f4a8f0`
  - mountain: `f46e7e1608f8de019599ed940c3aaf05d4a694638ac15a177297d4c979842768`
  - mixed: `60b6b33c39dded52b8414ea06eb560467586352b69c94b97b7a4106a679814e4`
- 基线轨迹 SHA-256：`12f25520d5961e584c5338565ad64c2058c5cbfc8cc02bd7d52224ca44fd0562`
- 演示轨迹 SHA-256：`bbccb3ff86203fad3459433ae03ba6f63c0f04707e45315c00da688f754958ff`

山地/混合样本的层级表出现 0..4 级混合，接缝状态均为 `closed`，匹配顶点修补后最大偏差为 `0.0`。精度由 `float64` 切换为 `float32` 时颜色像素差为 0，深度存在非零微小差异，符合“切换后重新渲染并报告差异”的要求。

CLI 全链路也已验证：`terrainviz generate`、`render`、`snapshot`、`recover` 均退出码 0；`recover` 从磁盘快照和高度场/材质文件成功重建地形与块层级。

## Windows 说明

Windows 安装步骤已写入 README，使用 python.org Python 3.11、`venv` 和 `pip install numpy`。本次实际测试不在 Windows 主机上执行，因此这里不声称已完成 Windows 实机验证；实现仅使用 `pathlib`、`os.replace`、`threading`、`struct`、`hashlib`、`json` 和 NumPy 等跨平台接口。

