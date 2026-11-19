-- dialect: postgres
SELECT DISTINCT ON ("e"."event_key")
  "e"."event_key" AS "event_key",
  "e"."event_ts" AS "event_ts",
  date_trunc('day', "e"."event_ts") AS "event_day",
  ("e"."received_seq" - 100) / 7 AS "seq_bucket",
  ("e"."received_seq" - 100) % 7 AS "seq_mod",
  "e"."user_id" AS "user_id",
  "e"."payload" AS "payload"
FROM "stream_events" AS "e"
WHERE "e"."event_ts" IS NOT NULL
ORDER BY "e"."event_key" ASC, "e"."event_ts" ASC, "e"."received_seq" ASC;

