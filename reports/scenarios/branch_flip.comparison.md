# Fixed vs float comparison: branch_flip

- Fixed final state: `5f9ae820860f4b32bdc4acb7d186495d4fee3dda7ffd9aa73f6d8a9b3ac39393`
- Float final state: `b9da28d2360c8da5c80ecb5611b89640e145e6241c0cffa64d752992b1a44159`
- Truth differences: 2
- Branch differences: 1
- Uncertain conditions: 2

## Explicit truth differences

| step | event | condition | fixed truth | fixed value | fixed error bound | float truth | float value | float error bound | earliest factor |
|---:|---|---|---|---:|---|---|---|---|---|
| 1 | edge | ratio_ok | True | {"format":"fixed","value":"0.297"} | {"format":"fixed","value":"0.000918260737"} | False | {"format":"float64","hex":"0x1.2f73f29dfe63ap-2","value":"0.29634074296299995"} | {"format":"float64","hex":"0x1.0000000000000p-52","value":"2.220446049250313e-16"} | edge.ratio_ok.target_precondition.expr.base |
| 1 | start | ratio_ok | True | {"format":"fixed","value":"0.297"} | {"format":"fixed","value":"0.000918260737"} | False | {"format":"float64","hex":"0x1.2f73f29dfe63ap-2","value":"0.29634074296299995"} | {"format":"float64","hex":"0x1.0000000000000p-52","value":"2.220446049250313e-16"} | start.ratio_ok.branch.expr.base |

## Branch differences

- Step 1: fixed `b_edge` -> `edge`, float `b_fallback` -> `fallback`.
