# 皮影剪纸屏风灯箱 · 离线软光栅验证组件

纯 Python（NumPy + Pillow）的确定性离线渲染器，用于在合入正式关卡前验证
五层红/橙/黄/绿/蓝剪纸屏风的透射、镂空投影与软阴影观感。无 GPU、无外部素材、
无网络依赖，所有样本由整数种子确定性生成。

## 安装

```
python -m pip install -r requirements.txt
```

## 一键演示

```
python run_demo.py
```

生成确定性样本、完整渲染、优雅中断/恢复、逐像素一致性核验以及双倍空气间隙
对照场景，产物写入 `demo_out/`。

## 分步命令

```
python -m paperlightbox.cli generate --scene scenes/sample.json \
    --gap-variant scenes/sample_gap2x.json --gap-scale 2.0
python -m paperlightbox.cli render --scene scenes/sample.json \
    --output out/run --snapshot out/snap
python -m paperlightbox.cli compare --scene scenes/sample.json \
    --reference-png out/a/lightbox.png --candidate-png out/b/lightbox.png \
    --reference-depth out/a/optical_depth.npy \
    --candidate-depth out/b/optical_depth.npy --diff-report out/diff.json
```

## 测试

```
python -m unittest discover -s tests -v
```

覆盖：确定性纹理/镂空、全部结构化错误码、顺序无关合成、分块快照损坏丢弃、
优雅中断与 SIGKILL 后续算逐像素一致、空气间隙投影位移与半影变宽、无硬黑边。

详见 `docs/TECHNICAL_NOTES.md`。
