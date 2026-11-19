SELECT
    vehicle_id,
    window_start(event_time, 300) AS window_start,
    COUNT(*) AS event_count,
    COUNT(DISTINCT trip_id) AS trip_count,
    SUM(mileage_delta) AS mileage_sum,
    SUM(battery_delta) AS battery_sum
FROM events
WHERE event_type IN ('payment', 'backfill') AND speed >= 0
GROUP BY vehicle_id, window_start

