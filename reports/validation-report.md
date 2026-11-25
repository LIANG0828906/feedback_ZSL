# 通道转换链路验证报告

- 结论: **PASS**
- 开始: 2026-11-25T11:03:16+08:00
- 结束: 2026-11-25T11:03:18+08:00
- 输出目录: `/tmp/channelpipe-final-Ab5439`
- SQLite: `/tmp/channelpipe-final-Ab5439/checkpoint-main.sqlite`

## 多版本计划

| 版本 | 版本字段 | 升级补齐数 | 语义一致 | 差异 |
|---:|---|---:|---|---|
| v1 | `plan_ver` | 21 | true | `` |
| v2 | `schemaVersion` | 21 | true | `` |
| v3 | `version` | 0 | true | `` |

负向解析:

- unknown-version: got `E_VERSION` field `plan_ver`, ok=true
- missing-version: got `E_SCHEMA` field `version`, ok=true
- missing-required-field-path: got `E_SCHEMA` field `plans[0].width`, ok=true

升级痕迹: 42 条；降级损失报告: 2 份。

## 异常隔离与续跑

- 注入分块: 1
- 首轮失败数: 1
- 首轮已提交: `[0 2 3 4 5]`
- 重试提交: `[1]`
- 最终提交序列: `[0 2 3 4 5 1]`
- 隔离: true；恢复: true

## 三路径一致性

- 图像: `color-320-rgba8`；分块数: 6；结论: true
- realtime-incremental: original seq=[0 1 2 3 4 5]; depth-edit seq=[0 1 2 3 4 5]; fill-alpha-edit seq=[0 1 2 3 4 5]; swizzle-edit seq=[0 1 2 3 4 5]; order-edit seq=[0 1 2 3 4 5]; 
- checkpoint-resume: original seq=[0 1 2 3 4 5]; depth-edit seq=[0 1 2 3 4 5]; fill-alpha-edit seq=[0 1 2 3 4 5]; swizzle-edit seq=[0 1 2 3 4 5]; order-edit seq=[0 1 2 3 4 5]; 
- full-recompute: original seq=[0 1 2 3 4 5]; depth-edit seq=[0 1 2 3 4 5]; fill-alpha-edit seq=[0 1 2 3 4 5]; swizzle-edit seq=[0 1 2 3 4 5]; order-edit seq=[0 1 2 3 4 5]; 

## 独立验证工具

- OK: true
- 真值指纹: true；快照身份: true
- 采样点: 36；阶段采样: 108；扫描像素: 4568064；丢弃记录: 108

## 资源边界

- 缓冲门阻塞等待: 20ms；已记录排队: true

