# Generated Samples

该目录由固定种子生成器创建：

```bash
python -m vector_workbench generate --out data --seed 20261123
```

- `scenes/`：3 份正常场景和 4 份异常场景。
- `truth/truth_manifest.json`：解析面积、路径包围盒、闭合检查点、曲线采样和样式契约。
- `samples.json`：场景索引、类别和期望异常码。

样本是可重复生成产物。不要手工修改后继续引用旧真值哈希；应重新运行生成器。

