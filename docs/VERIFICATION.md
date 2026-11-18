# 实际验证记录

验证日期：2026-11-18。环境：Python 3.14.4、NumPy 2.5.5、Pillow 13.0.0，
Linux，本地离线运行。

## 自动化测试

命令：

```bash
python3 -m unittest discover -s tests -v
```

结果：`Ran 13 tests in 9.200s`，`OK`。

覆盖项：

- 编织纹理同参数逐字节一致，且不受全局 NumPy RNG 影响。
- 八类非法场景分别得到指定业务码和层号；CLI 非零退出且不产生
  `rgba.png`/`snapshot/`。
- 字符串/非负整数层编号与光源编号均可规范化并参与重复检测。
- 完整渲染重复运行 `rgba.png`、层归属图、投影清单和 `report.json`
  逐字节一致。
- 图像所有像素满足预乘 Alpha 约束，且正对光源样例的最大 RGB 小于 255。
- 投影半影对接收面距离、光源到花格距离均单调；边缘亮度从遮挡侧向亮侧
  单调上升。
- 改变花格层间距或点光源位置后，报告落点坐标随之改变。
- 真实 SIGKILL 发生在部分块提交后，重启续算与不中断基线逐像素一致，
  `report.json` 逐字节一致。
- Ctrl+C 完成当前块、写 `recovery.json`、退出码 0；继续运行后结果与
  基线一致。
- 人为破坏已提交 tile 后，重启识别校验和失败、整体丢弃快照并重算；
  结果仍与基线一致。
- 修改一个像素一个通道后，比较命令返回 3，差异项包含块号、坐标和通道。

## 样例渲染

场景：`examples/courtyard_night.json`

输出：`examples/verified_output/`

尺寸：256x160，块 64x40，共 16 块，相机 2x2 子像素。

```bash
python3 -m veilrender render \
  --scene examples/courtyard_night.json \
  --output-dir examples/verified_output \
  --no-resume --quiet
```

最终摘要：

| 工件 | SHA-256 |
|---|---|
| `rgba.png` | `57ee8687c37aa8f5633f0d02abb8c58489583ebba4026f082143901f4967d559` |
| `layer_ownership.png` | `74c6a0e6334a7cd05e0e20035f3043e5a5b5e6ab9d722e906605116f66948da5` |
| `projection_landings.json` | `814ec42283f8700323cb83976e847f74dc117fda19971591aa4e35594a221d4f` |
| `report.json` | `10a244e19e832be79da5ba2afd120bd1f9b4124d272e9ca755efcd7c416e45cb` |
| `snapshot/manifest.json` | `70cc460aeea2611ea380ddeb008e51414b4af6c9afdefeaba22e91f6eb8557d8` |

`report.json` 记录：

- 快照提交数/总块数：`16 / 16`
- `rgba.png` 最大 RGB 为 127，纯白像素数为 0，所有像素满足 `RGB <= A`
- 花格投影落点数：40；接收面内 17；每个光源均有独立落点清单
- 半影接收面距离序列：`0.01139, 0.01211, 0.01355, 0.01625`
- 半影光源距离序列：`0.0129975, 0.0133375, 0.013975, 0.01525`
- 两项单调性检查：均为 `true`

每层统计（完整值见 `examples/verified_output/report.json`）：

| layer | id | kind | 可见片元 | 平均透射率 | 透射占比 | 反射占比 |
|---:|---|---|---:|---:|---:|---:|
| 1 | inner-teal-gauze | gauze | 60536 | 0.508939531 | 0.116407910 | 0.883592090 |
| 2 | inner-silver-lattice | lattice | 66420 | 0.835975143 | 0.505756506 | 0.494243494 |
| 3 | middle-rose-gauze | gauze | 89814 | 0.395172704 | 0.347076108 | 0.652923892 |
| 4 | rose-gauze-pair | gauze | 110460 | 0.363635983 | 0.261416365 | 0.738583635 |
| 5 | outer-bronze-lattice | lattice | 120124 | 0.837323191 | 0.597090366 | 0.402909634 |
| 6 | outer-rose-gauze | gauze | 146944 | 0.246529911 | 0.204615354 | 0.795384646 |

完整快照已存在时再次执行同一 `render` 命令，仅用约 0.043 秒重新组装工件；
`compare` 返回 `difference_count: 0`。清除输出后使用 `--no-resume` 重画，
五个最终工件摘要保持不变。

## 中断验证输出位置

自动化测试在临时目录中生成并销毁中断样例。强杀时已提交块数小于总块数，
恢复后最终 `rgba.png`、`layer_ownership.png`、投影 JSON 和 `report.json`
均与基线相同。`recovery.json`、`run_metrics.json` 和
`differences.json` 的字段含义见 `docs/TECHNICAL.md`。

