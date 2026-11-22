# 验收报告 - 自适应抖动缓冲工作台

- 样本: voice_seed42 (seed=42, 300 帧, 295 条到达记录)
- 抖动模式: delay,drop,duplicate,reorder,surge
- 内容哈希: c09f0adf73d22071
- 策略版本: 986382e894d7e015

## 三路径一致性
- 实时接收 vs 离线重放 vs 断点恢复: 逐样本一致
- 提交窗口: 300, 断音: 12, 平均排队: 110.6ms, 最终目标延迟: 180ms
- 重复帧丢弃: 5, 迟到帧丢弃: 2

## 并发与串行一致性
- workers=1: 一致
- workers=2: 一致
- workers=4: 一致
- workers=8: 一致

## 断点恢复
- stop=1: 一致
- stop=50: 一致
- stop=137: 一致
- stop=250: 一致

## 错误码链路
- E_META: OK
- E_JITTER: OK
- E_DECODE: OK
- E_ORDER: OK
- E_DUP: OK
- E_PARA: OK

## 目标延迟决策序列
- t=150ms water_high_low_latency 120->100ms (water 13 > 12 and late ratio 0.000 < 0.020)
- t=1100ms jitter_surge 100->100ms (delay std 16.9ms > 2.5x baseline 6.1ms; no gaps -> hold low latency)
- t=1220ms late_ratio_high 100->120ms (late ratio 0.150 > 0.100; water trend 3.0)
- t=1270ms late_ratio_high 120->140ms (late ratio 0.150 > 0.100; water trend -1.0)
- t=1300ms jitter_surge 140->160ms (delay std 34.7ms > 2.5x baseline 6.3ms; 3 gaps in window -> prefer continuity)
- t=1350ms late_ratio_high 160->180ms (late ratio 0.150 > 0.100; water trend 0.0)
- t=1400ms late_ratio_high 180->200ms (late ratio 0.150 > 0.100; water trend 0.0)
- t=1450ms late_ratio_high 200->220ms (late ratio 0.150 > 0.100; water trend 0.0)
- t=1500ms late_ratio_high 220->240ms (late ratio 0.150 > 0.100; water trend 0.0)
- t=1560ms water_high_low_latency 240->220ms (water 21 > 12 and late ratio 0.000 < 0.010)
- t=1610ms water_high_low_latency 220->200ms (water 18 > 12 and late ratio 0.000 < 0.020)
- t=1660ms water_high_low_latency 200->180ms (water 16 > 12 and late ratio 0.000 < 0.020)
- t=1710ms water_high_low_latency 180->160ms (water 15 > 12 and late ratio 0.000 < 0.020)
- t=2070ms water_high_low_latency 160->140ms (water 14 > 12 and late ratio 0.000 < 0.020)
- t=2150ms water_high_low_latency 140->120ms (water 13 > 12 and late ratio 0.000 < 0.020)
- t=3060ms late_ratio_high 120->140ms (late ratio 0.150 > 0.100; water trend 0.0)
- t=3110ms late_ratio_high 140->160ms (late ratio 0.150 > 0.100; water trend -1.0)
- t=3160ms late_ratio_high 160->180ms (late ratio 0.200 > 0.100; water trend -1.0)

## 结论
- **PASS**
