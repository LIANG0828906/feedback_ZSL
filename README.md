# 离线转台母带渲染组件

面向实时过场动画对白/音效多轨素材的确定性离线渲染器。全部 WAV 素材与工程
清单均由代码按整数种子合成，不依赖外部媒体、网络或真实录音；同参数重复运行
生成的素材、快照与立体声母带逐字节一致。

## 环境准备

- Windows 10/11、Linux 或 macOS，Python 3.10+（本机验证使用 Python 3.14）
- 依赖 NumPy、SciPy、soundfile（soundfile 需可调用 libsndfile 后端）

```bash
python -m venv .venv
# Linux/macOS
. .venv/bin/activate
# Windows
.venv\Scripts\activate
pip install -r requirements.txt
```

## 快速开始

```bash
# 1. 生成确定性示例工程（manifest.json + stems/*.wav）
python -m offline_master.demo init demo_project

# 2. 离线渲染立体声母带与统计报告
python -m offline_master.demo render demo_project/manifest.json --out demo_out

# 3. 触发全部清单校验规则的反面示例（退出码 2）
python -m offline_master.demo invalid-demo demo_invalid
```

渲染产物：

- `master.wav`：PCM-16 立体声母带，采样数等于清单时长
- `report.json`：对齐、相位、双耳方位、响度/真峰值与限幅统计
- `snapshot/`：分块快照、恢复报告，支持中断续算

## 业务码

| 业务码 | 含义 | 是否致命 | 退出码 |
| --- | --- | --- | --- |
| `E_MANIFEST_INVALID` | 清单或 WAV 校验失败，告警逐条汇总不短路 | 是 | 2 |
| `E_TRANSIENT_NOT_FOUND` | 某轨瞬态检测失败（如环境床），其余轨继续 | 否 | 0 |
| `E_RESUME_MISMATCH` | 快照摘要/参数与当前输入不一致，丢弃重算 | 重算后成功 | 3（直接命中时） |

## 处理流水线

1. `manifest.py` 全量校验清单与 WAV（采样率/位深/声道、锚点范围、角度与
   对白点升序、素材类型与文件去重），错误聚合为含文件名的结构化告警。
2. `synthesis.py` 按种子生成对白（爆破音+元音谐波）、脚步（脚跟+脚尖冲击）、
   衣料（宽带摩擦噪声）与环境（持续低电平床）；同组轨共享确定性节奏脉冲。
3. `alignment.py` 在画面对白点邻域做极性无关的单采样瞬态检测与整数偏移。
4. `spatial.py` 用解析 ITD/ILD 模型逐角度计算双耳增益与延迟，含稳定性与
   主声像方位真值审计；流式可变延迟线保证角度连续、无整侧翻转。
5. `phase.py` 在 20–120 Hz 低频段估计组内相对延迟与极性并校正，提升低频
   能量且瞬态位移不超过 1 采样。
6. `loudness.py` 按 ITU-R BS.1770 计算积分响度，4 倍过采样测真峰值，迭代
   “响度归一化 + 真峰限幅”，长度不变且不移动瞬态。
7. `pipeline.py` 固定块渲染并原子替换提交快照；Ctrl+C 完成当前块后退出 0，
   重启仅依赖快照续算，结果与一次性渲染逐字节一致。

## 测试

```bash
python -m pytest
```

20 个测试覆盖清单校验、确定性合成、瞬态对齐、双耳稳定、相位校正、响度合规、
快照中断/续算与 `E_RESUME_MISMATCH`。实测结果见 `TECH_NOTES.md`。
