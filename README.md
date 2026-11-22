# 图像分割与掩码处理工作台（SegWorkbench）

面向游戏与多媒体美术素材的本地、确定性图像分割与掩码处理工作台。
覆盖「确定性样本 → 区域生长 → 阈值化/形态学清理 → 几何统计与轮廓 →
分块状态机 → 原子检查点 → 独立验证 → 异常注入 → 中断恢复」完整链路，
不使用容器、数据库或外部服务。

## 快速开始

```bash
# 1. 建立虚拟环境并安装固定版本依赖（离线时加 --no-index --find-links wheelhouse）
python -m venv .venv
.venv/bin/pip install -r requirements-gui.txt        # 仅核心计算可用 requirements.txt

# 2. 生成确定性样本（图像 + 解析真值清单）到 data/generated
.venv/bin/python -m segworkbench.cli generate

# 3. 运行分块分割
.venv/bin/python -m segworkbench.cli run \
  --sample-id sample_01_blocks --checkpoint-dir workdir/run1

# 4. 独立验证（只读持久化产物与真值指纹）
.venv/bin/python -m segworkbench.cli verify \
  --sample-id sample_01_blocks --checkpoint-dir workdir/run1

# 5. 端到端示例：正常路径 + 四类注入 + 中断恢复 + 版本作废
.venv/bin/python examples/run_demo.py

# 6. 测试
.venv/bin/python -m unittest discover -s tests
```

Windows PowerShell 把 `.venv/bin/` 换成 `.venv\Scripts\`；无头环境渲染
GUI 前设置 `QT_QPA_PLATFORM=offscreen`。

## 目录

```
segworkbench/
  core/         错误码、版本常量
  utils/        SHA-256、PNG 编解码、日志
  pipeline/     样本生成/加载、生长、形态学、几何、状态机、引擎、并发
  persistence/  原子串行检查点
  validation/   独立验证器（不导入引擎）
  gui/          可选 PyQt5 查看器
  cli.py        generate / run / verify 命令行
examples/       端到端示例与注入配置
tests/          24 个 unittest 用例
docs/           ARCHITECTURE.md、VALIDATION_RESULTS.md
wheelhouse/     离线 wheel 缓存
```

## 关键性质

- **逐像素确定性**：固定种子、固定 BFS 顺序、纯 numpy 计算；串行/并发/
  恢复路径掩码 SHA-256 一致。
- **稳定错误码**：`E_DECODE/E_META/E_PARA/E_SEED/E_SPILL/E_REGION/
  E_BLOCK/E_STATE/E_VERSION/E_HASH/E_CANCELLED`，全部由输入文件或参数
  注入触发，注入点在日志可追溯，无需改源码。
- **提交冻结**：块阶段四态状态机，已提交产物只读；参数调整只影响未提交块。
- **版本一致恢复**：图像哈希、参数版本、算法版本任一不匹配则整份快照
  作废，禁止混用旧块。
- **独立验证**：验证器自带形态学实现，从持久化产物独立重算并定位首个
  不一致像素（全局坐标 + 块编号 + 阶段）。

实测数据见 `docs/VALIDATION_RESULTS.md`，设计细节见
`docs/ARCHITECTURE.md`。
