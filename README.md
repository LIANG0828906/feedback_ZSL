# Vector Boolean Studio

面向游戏 UI 素材管线的本地矢量路径布尔合成组件。支持 v1/v2 路径规则、直线/
二次/三次贝塞尔、孔洞、近重合边、极短退化段、四种布尔运算、误差报告、
确定性填充预览、协作式取消与原子工程恢复。

几何核心、CLI、测试和工程恢复均为纯 Python；桌面窗口使用 PySide6。

## Windows 安装

目标环境：Windows 10/11、Python 3.11、pip。

```powershell
python -m pip install --upgrade pip
.\scripts\prepare_offline_wheels.ps1
python -m pip install --no-index --find-links vendor/wheels -r requirements.txt
```

若已有内网缓存，也可先确认 wheel 齐全再离线安装。固定版本：

- `PySide6==6.8.2.1`
- `numpy==2.2.3`

几何核心实际只依赖标准库；numpy/PySide6 不参与解析和布尔判定。

## 快速运行

首次运行自动生成确定性 v1/v2 样本：

```powershell
python scripts\generate_samples.py --root samples
```

无桌面或自动化环境：

```powershell
python -m unittest discover -v
python -m vpipe acceptance --samples samples --artifacts artifacts
python -m vpipe run --a samples\v1\base_a.json --b samples\v1\base_b.json `
  --operation union --tolerance 0.01 `
  --out-result artifacts\union.v2.json `
  --project artifacts\union.vproj `
  --preview artifacts\union.ppm `
  --log artifacts\run_trace.jsonl
```

有桌面会话：

```powershell
python -m vpipe.ui
```

界面提供路径选择、运算类型、容差与阈值、运行/取消、预览、工程保存/载入、
v1/v2 导出。预览刷新只读取不可变的最近提交快照；失败或取消不清空上一有效
预览。

## 关键行为

- v1 按轮廓嵌套深度推导绕向：偶数深度 `ccw`，奇数深度 `cw`；必要时纠正
  记录绕向并标注推导来源。
- v2 显式记录方向、分组、顶点共享表和统一容差；共享索引/坐标不符返回
  `E_SHARE_INVALID` 并定位子轮廓。
- v2 降级 v1 不静默丢字段：返回 `E_VERSION_LOSS`，列出方向、分组、共享、
  容差、轨迹、元数据等全部受影响项。
- 曲线求交输出参数、切向、角度、置信级别；容差带内无法归类返回
  `uncertain`，不冒充 `intersect`。
- 布尔边按左右隶属变化选择，每个保留边可追溯到来源 operand/轮廓/段/参数
  区间；无法消除的自交以 `E_SELF_INTERSECT` 拒绝提交。
- 同一几何对并发提交返回 `E_BUSY`；取消在求交/分段/分类边界响应，最后已
  提交结果保持不变。
- 误差以输入操作真值栅格和双向最近边界评估，报告面积相对偏差、边界上界、
  容差放大关系；超阈值结果为降级且禁止导出。
- 光栅预览在相同轮廓与网格下逐字节一致；预览哈希不在提交清单或不是最新时
  返回 `E_PREVIEW_STALE`。
- 工程保存使用同目录临时文件、`fsync`、`os.replace`；载入校验格式版本与内容
  SHA-256，不匹配返回 `E_PROJECT_MISMATCH`。

## 文档

- `docs/FORMATS.md`：v1/v2 与工程文件格式、降级语义。
- `docs/ARCHITECTURE.md`：容差传播、布尔拼接、并发/提交、预览、误差与错误码。
- `docs/VERIFICATION.md`：实际执行命令、24 项测试结果、验收面积/误差表与
  可复现轨迹位置。

确定性样本位于 `samples/`，本次实际轨迹位于 `artifacts/trace.jsonl`，
机器可读验收报告位于 `artifacts/acceptance_report.json`。

## 资源与恢复边界

取消和异常路径只在局部缓冲中保存中间边/环，不写工程、不替换已提交结果。
进程被强制终止时不承诺生成退出报告；下次只依据磁盘上通过哈希校验且标记为
已提交的工程恢复。

