# 本地确定性样本

本目录由以下命令生成：

```bash
.venv/bin/python -m sciseq generate --out samples --overwrite
```

- `small`：320×384，24 帧，种子 1101
- `medium`：512×640，56 帧，种子 2202
- `large`：768×1024，96 帧，种子 3303

每组使用 `manifest.json` 描述帧文件、尺寸、哈希和真值文件；`truth.json` 保存每帧亚像素位移、噪声基底、亮斑/线段参数与逐帧特征中心；`clean_base.npy` 是无噪声参考基准图。

不要手工修改帧文件或清单哈希，否则加载器会按设计返回 `E_META` 或 `E_DECODE`。

