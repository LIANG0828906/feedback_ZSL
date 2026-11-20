# 实际验证结果记录

验证环境：Python 3.14.4（满足 ≥3.11）、NumPy 2.5.5（用户态 pip 安装）、
无 Tkinter/无图形显示（GUI 走优雅降级，仅验证脚本化入口）、无网络服务依赖。

## 自动化测试

命令：`python3 -m unittest discover -s tests`

结果：**34 个测试全部通过（OK）**，覆盖：

1. 输入解析与严格校验（缺字段、步号越界、负用量、未知玩家、
   客户端序号重复、布尔冒充数字、未知策略、首个出错位置定位）；
2. `reject`/`degrade` 两种裁决、按客户端序号的可归因拒绝、
   到达顺序无关、在线逐步与离线整批结果一致、时间戳不进哈希；
3. `E_QUOTA_EXCEEDED` 严格模式错误码与完整归因上下文；
4. 检查点原子写、写失败保留上一有效检查点并清理临时文件、
   水位与哈希链前缀字段、版本不符/字段缺失/断链/篡改的
   `E_RESUME_INVALID` 与首个断链步定位；
5. 并行度 1/2/3/5 下逐演练逐步哈希完全相同、轨迹日志逐字节一致、
   生成器同种子重复生成逐字节一致、基线分叉首个不一致定位、
   `E_BASELINE_VERSION` 版本闸门、暂停/继续与协作式取消；
6. CLI 端到端（generate → replay → verify → resume）与非法输入退出码。

## 并行度无关确定性

同批 3 场演练分别以并行度 1/2/4/8 重演，三场演练末步哈希前缀：

| 演练 | 各并行度末步哈希前缀（完全相同） |
| --- | --- |
| large_degrade | `fafc019cc1d7` |
| medium_reject | `f4f599be798f` |
| small_reject | `ef66272f95b3` |

`diff -r` 对比并行度 1 与 8 的 `trajectories/` 目录：**逐字节一致**。
`verify` 输出：`{"exercises":3,"status":"MATCH"}`。

## 强制终止续算

命令：`python3 -m scripts.verify_kill_resume`

子进程在第 5 步提交检查点后对自己发 `SIGKILL`；续算进程从第 5 步
检查点恢复并跑完 40 步，与一次完整离线重演对比：

- 40 个步骤哈希链完全相同；
- 最终各玩家累计配额与累计惩罚完全相同；
- 已提交的 5 步未被重复累加。

## 非法输入示例

`required_compute = -2.0` 时返回：

```
E_INPUT_INVALID | operations[0].required_compute |
非法输入 @ operations[0].required_compute: 字段 'required_compute' 不能为负，实际为 -2.0
```

## GUI 说明

当前容器无 `python3-tk` 且无显示会话，`replay_service.gui.launch`
抛出明确的 `RuntimeError` 提示改用 CLI；在 Windows 桌面会话中执行
`python -m replay_service.cli gui --sample-dir samples/run1 --work-dir <dir>`
即可打开复核界面。
