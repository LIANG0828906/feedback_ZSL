# Local Persistent State

- `var/checkpoints/` 保存工作台阶段检查点、快照和版本清单。
- 可写性要求：桌面应用启动前目录必须存在且当前用户可写。
- 快照目录中的 manifest 与 payload 由应用原子写入。
- 版本不匹配或 payload 哈希不匹配时，应用会整体作废旧快照。
- 运行生成内容默认不纳入 Git；保留 `.gitkeep` 以维持目录。

