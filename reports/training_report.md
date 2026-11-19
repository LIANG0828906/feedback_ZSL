# Asynchronous training verification

Status: **pass**

## Results

- Steps: 300 / 300
- Final max parameter deviation: 0.000e+00
- Max loss relative error: 0.000e+00
- Gradient recompute max deviation: 0.000e+00
- Step update max deviation: 0.000e+00
- Accuracy trained/reference: 1.000000 / 1.000000
- Stale rejections: 4
- Duplicate submissions discarded: 1
- Completed takeovers: 1
- Max worker submit P95: 10.032 ms
- End-to-end: 10.359 s

## Affected Records

- step=76 source=stale_gradient_rejected_before_routing parameters=[weight.000.00, weight.000.01, weight.000.02, weight.000.03, weight.000.04, ...]
- step=76 source=stale_gradient_rejected_before_routing parameters=[weight.000.00, weight.000.01, weight.000.02, weight.000.03, weight.000.04, ...]
- step=76 source=stale_gradient_rejected_before_routing parameters=[weight.000.00, weight.000.01, weight.000.02, weight.000.03, weight.000.04, ...]
- step=76 source=stale_gradient_rejected_before_routing parameters=[weight.000.00, weight.000.01, weight.000.02, weight.000.03, weight.000.04, ...]
- step=45 source=duplicate_gradient_discarded parameters=[]
- step=105 source=parameter_shard_takeover_from_latest_committed_state parameters=[bias.00, bias.01, bias.03, bias.05, bias.06, ...]

## Checks

- PASS `accuracy_not_below_reference`
- PASS `all_steps_present`
- PASS `complete_routes`
- PASS `complete_step_submissions`
- PASS `current_parameters_match_last_history`
- PASS `final_parameter_tolerance`
- PASS `gradient_recompute_tolerance`
- PASS `lag_event_marker_consistent`
- PASS `loss_sequence_tolerance`
- PASS `ownership_complete`
- PASS `ownership_unique`
- PASS `step_transaction_history_complete`
- PASS `step_update_tolerance`
- PASS `takeover_recovery_within_30s`
- PASS `worker_p95_within_400ms`
