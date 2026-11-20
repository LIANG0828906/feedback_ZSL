# 验证记录

环境：

```text
OS: Ubuntu 26.04 (WSL2)
OpenJDK: 17.0.21.1
Maven: 3.9.9
JUnit Jupiter: 5.10.2
```

执行命令：

```bash
scripts/build.sh -q clean package
scripts/demo.sh > verification/demo-output.jsonl
diff -u <第一次 demo 输出> <第二次 demo 输出>
```

结果：

- 13 个 JUnit 5 测试全部通过。
- 演示 8 个场景全部通过，4 个故障点均以退出码 137 终止后可恢复。
- 连续两次完整演示输出无差异。
- `verification/demo-output.jsonl` 是最终构建的实际输出。
