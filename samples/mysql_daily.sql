-- dialect: mysql
SELECT
  DATE(`s`.`event_ts`) AS `sale_day`,
  `st`.`region` AS `region`,
  `p`.`category` AS `category`,
  SUM(`s`.`amount` DIV 7) AS `amount_div_7`,
  SUM(`s`.`qty` MOD 3) AS `qty_mod_3`,
  COUNT(*) AS `sale_count`,
  AVG(`s`.`amount`) AS `avg_amount`
FROM `fact_sales` AS `s`
JOIN `dim_store` AS `st`
  ON `s`.`store_id` = `st`.`store_id`
JOIN `dim_product` AS `p`
  ON `s`.`product_id` = `p`.`product_id`
WHERE `s`.`amount` >= -500
  AND `st`.`active` = 1
GROUP BY DATE(`s`.`event_ts`), `st`.`region`, `p`.`category`
ORDER BY `sale_day` ASC, `region` ASC, `category` ASC;

