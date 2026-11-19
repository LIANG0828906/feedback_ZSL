SELECT
    vehicle_id,
    date_trunc('hour', event_time) AS window_start,
    SUM(mileage_delta) AS mileage_delta,
    SUM(energy_delta) AS energy_delta,
    ROUND(SUM(energy_delta) * 100.0 / NULLIF(SUM(mileage_delta), 0), 6) AS energy_per_100km
FROM vehicle_events
WHERE partition_date >= DATE '2026-01-01'
  AND partition_date < DATE '2026-01-04'
  AND event_time >= TIMESTAMP '2026-01-01 00:00:00'
  AND event_time < TIMESTAMP '2026-01-04 00:00:00'
GROUP BY vehicle_id, date_trunc('hour', event_time)
ORDER BY vehicle_id, window_start;

