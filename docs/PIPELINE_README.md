# afpipe — 音频指纹与相似片段检索管线

面向游戏与互动媒体素材库的本地数据处理管线：多格式解码归一、分帧指纹、
版本化持久化索引、相似片段检索、片段族归并、断点恢复与结果验证。

## 快速开始

```bash
python3 -m venv .venv
source .venv/bin/activate            # Windows: .venv\Scripts\activate
pip install -r requirements.txt
# 需要本机 ffmpeg（PATH、FFMPEG 环境变量或 imageio-ffmpeg）

python run_acceptance.py             # 端到端验收，报告写入 artifacts/
python -m unittest discover -s . -p "test_*.py"
```

## CLI

```bash
python -m afpipe generate --out workspace/corpus
python -m afpipe build    --manifest workspace/corpus/manifest.json \
                          --work workspace/work --index workspace/index
python -m afpipe search   --manifest workspace/corpus/manifest.json \
                          --index workspace/index --report search.json
python -m afpipe verify   --manifest workspace/corpus/manifest.json \
                          --index workspace/index
python -m afpipe compat   --manifest workspace/corpus/manifest.json \
                          --work workspace/work --index workspace/index
python -m afpipe checkpoint-info --manifest workspace/corpus/manifest.json \
                          --work workspace/work
```

所有参数（采样率、频带数、量化位、帧长帧移、并行度、分片大小、预算等）
均可通过 CLI 标志覆盖，越界返回 `E_PARA`。设计细节与实测数据见
[TECHNICAL_NOTES.md](TECHNICAL_NOTES.md)。

## 版本兼容原则

索引携带格式版本、参数版本、算法版本与清单指纹；任一不匹配返回 `E_VER`
并拒绝查询。旧版 1.0.0 索引只能经显式升级进入 2.0.0，升级映射写入
`upgrade_trail.json`，不存在静默混用或降级。
