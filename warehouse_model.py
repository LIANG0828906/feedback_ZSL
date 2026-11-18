"""Deterministic warehouse model shared by the generator and drift simulator."""

from __future__ import annotations

from collections import OrderedDict


TABLE_LAYERS = OrderedDict(
    [
        ("raw_customers", "raw"),
        ("raw_products", "raw"),
        ("raw_orders", "raw"),
        ("raw_order_items", "raw"),
        ("detail_orders", "detail"),
        ("detail_order_items", "detail"),
        ("summary_daily_sales", "summary"),
        ("summary_customer_sales", "summary"),
        ("summary_product_sales", "summary"),
        ("summary_channel_sales", "summary"),
        ("summary_region_sales", "summary"),
        ("summary_finance", "summary"),
    ]
)

TABLE_DESCRIPTIONS = {
    "raw_customers": "Customer source snapshot",
    "raw_products": "Product source snapshot",
    "raw_orders": "Order header source snapshot",
    "raw_order_items": "Order line source snapshot",
    "detail_orders": "Conformed order detail",
    "detail_order_items": "Conformed order line detail",
    "summary_daily_sales": "Daily sales aggregate",
    "summary_customer_sales": "Customer sales aggregate",
    "summary_product_sales": "Product sales aggregate",
    "summary_channel_sales": "Channel sales aggregate",
    "summary_region_sales": "Region and segment sales aggregate",
    "summary_finance": "Finance aggregate by currency",
}


TABLE_SCHEMA = OrderedDict(
    [
        (
            "raw_customers",
            [
                ("customer_id", "INTEGER"),
                ("customer_name", "TEXT"),
                ("region", "TEXT"),
                ("segment", "TEXT"),
                ("signup_date", "TEXT"),
            ],
        ),
        (
            "raw_products",
            [
                ("product_id", "INTEGER"),
                ("product_name", "TEXT"),
                ("category", "TEXT"),
                ("list_price", "REAL"),
                ("cost", "REAL"),
            ],
        ),
        (
            "raw_orders",
            [
                ("order_id", "INTEGER"),
                ("customer_id", "INTEGER"),
                ("channel", "TEXT"),
                ("status", "TEXT"),
                ("order_ts", "TEXT"),
                ("currency", "TEXT"),
            ],
        ),
        (
            "raw_order_items",
            [
                ("order_id", "INTEGER"),
                ("line_no", "INTEGER"),
                ("product_id", "INTEGER"),
                ("quantity", "INTEGER"),
                ("unit_price", "REAL"),
                ("discount_amount", "REAL"),
                ("tax_amount", "REAL"),
                ("coupon_code", "TEXT"),
            ],
        ),
        (
            "detail_orders",
            [
                ("order_id", "INTEGER"),
                ("customer_id", "INTEGER"),
                ("order_date", "TEXT"),
                ("channel", "TEXT"),
                ("status", "TEXT"),
                ("item_count", "INTEGER"),
                ("gross_amount", "REAL"),
                ("discount_amount", "REAL"),
                ("tax_amount", "REAL"),
                ("net_amount", "REAL"),
                ("customer_region", "TEXT"),
                ("customer_segment", "TEXT"),
            ],
        ),
        (
            "detail_order_items",
            [
                ("order_id", "INTEGER"),
                ("line_no", "INTEGER"),
                ("product_id", "INTEGER"),
                ("quantity", "INTEGER"),
                ("unit_price", "REAL"),
                ("gross_amount", "REAL"),
                ("discount_amount", "REAL"),
                ("tax_amount", "REAL"),
                ("net_amount", "REAL"),
                ("category", "TEXT"),
                ("coupon_code", "TEXT"),
            ],
        ),
        (
            "summary_daily_sales",
            [
                ("order_date", "TEXT"),
                ("orders", "INTEGER"),
                ("units", "INTEGER"),
                ("gross_sales", "REAL"),
                ("net_sales", "REAL"),
                ("tax_amount", "REAL"),
            ],
        ),
        (
            "summary_customer_sales",
            [
                ("customer_id", "INTEGER"),
                ("region", "TEXT"),
                ("segment", "TEXT"),
                ("orders", "INTEGER"),
                ("net_sales", "REAL"),
            ],
        ),
        (
            "summary_product_sales",
            [
                ("product_id", "INTEGER"),
                ("category", "TEXT"),
                ("units", "INTEGER"),
                ("gross_sales", "REAL"),
                ("net_sales", "REAL"),
            ],
        ),
        (
            "summary_channel_sales",
            [
                ("channel", "TEXT"),
                ("orders", "INTEGER"),
                ("net_sales", "REAL"),
            ],
        ),
        (
            "summary_region_sales",
            [
                ("region", "TEXT"),
                ("segment", "TEXT"),
                ("orders", "INTEGER"),
                ("net_sales", "REAL"),
            ],
        ),
        (
            "summary_finance",
            [
                ("currency", "TEXT"),
                ("gross_sales", "REAL"),
                ("net_sales", "REAL"),
                ("tax_amount", "REAL"),
                ("discount_amount", "REAL"),
            ],
        ),
    ]
)


REPORT_CONFIG = [
    {
        "report_id": "rpt_daily_sales",
        "report_name": "Daily Sales Operations",
        "owner": "sales_ops",
        "task_name": "summary_daily_sales_publish",
        "description": "Daily sales operations dashboard",
    },
    {
        "report_id": "rpt_customer_sales",
        "report_name": "Customer Revenue",
        "owner": "crm_analytics",
        "task_name": "summary_customer_sales_publish",
        "description": "Customer revenue and segmentation report",
    },
    {
        "report_id": "rpt_product_sales",
        "report_name": "Product Performance",
        "owner": "merchandising",
        "task_name": "summary_product_sales_publish",
        "description": "Product category performance report",
    },
    {
        "report_id": "rpt_channel_sales",
        "report_name": "Channel Performance",
        "owner": "growth",
        "task_name": "summary_channel_sales_publish",
        "description": "Channel conversion and revenue report",
    },
    {
        "report_id": "rpt_region_sales",
        "report_name": "Regional Sales",
        "owner": "regional_ops",
        "task_name": "summary_region_sales_publish",
        "description": "Regional and segment sales report",
    },
    {
        "report_id": "rpt_finance",
        "report_name": "Finance Close",
        "owner": "finance",
        "task_name": "summary_finance_publish",
        "description": "Finance close and reconciliation report",
    },
    {
        "report_id": "rpt_executive",
        "report_name": "Executive Revenue",
        "owner": "executive",
        "task_name": "summary_daily_sales_publish",
        "description": "Executive revenue overview",
    },
    {
        "report_id": "rpt_executive",
        "report_name": "Executive Revenue",
        "owner": "executive",
        "task_name": "summary_finance_publish",
        "description": "Executive revenue overview",
    },
]


METRIC_CONFIG = [
    {
        "metric_id": "total_net_sales",
        "metric_name": "Total net sales",
        "source_table": "summary_finance",
        "query_sql": (
            "SELECT ROUND(COALESCE(SUM(net_sales), 0), 2) AS metric_value "
            "FROM summary_finance"
        ),
        "threshold_pct": 0.0,
        "description": "Sum of net sales across finance currencies",
    },
    {
        "metric_id": "total_gross_sales",
        "metric_name": "Total gross sales",
        "source_table": "summary_daily_sales",
        "query_sql": (
            "SELECT ROUND(COALESCE(SUM(gross_sales), 0), 2) AS metric_value "
            "FROM summary_daily_sales"
        ),
        "threshold_pct": 0.0,
        "description": "Sum of daily gross sales",
    },
    {
        "metric_id": "units_sold",
        "metric_name": "Units sold",
        "source_table": "summary_product_sales",
        "query_sql": (
            "SELECT COALESCE(SUM(units), 0) AS metric_value "
            "FROM summary_product_sales"
        ),
        "threshold_pct": 0.0,
        "description": "Total product units sold",
    },
    {
        "metric_id": "order_count",
        "metric_name": "Order count",
        "source_table": "summary_channel_sales",
        "query_sql": (
            "SELECT COALESCE(SUM(orders), 0) AS metric_value "
            "FROM summary_channel_sales"
        ),
        "threshold_pct": 0.0,
        "description": "Total non-cancelled orders",
    },
    {
        "metric_id": "average_order_value",
        "metric_name": "Average order value",
        "source_table": "summary_channel_sales",
        "query_sql": (
            "SELECT ROUND(COALESCE(SUM(net_sales), 0) / "
            "NULLIF(SUM(orders), 0), 2) AS metric_value "
            "FROM summary_channel_sales"
        ),
        "threshold_pct": 0.0,
        "description": "Net sales divided by order count",
    },
    {
        "metric_id": "tax_amount",
        "metric_name": "Tax amount",
        "source_table": "summary_finance",
        "query_sql": (
            "SELECT ROUND(COALESCE(SUM(tax_amount), 0), 2) AS metric_value "
            "FROM summary_finance"
        ),
        "threshold_pct": 0.0,
        "description": "Sum of tax across finance currencies",
    },
    {
        "metric_id": "discount_ratio_pct",
        "metric_name": "Discount ratio",
        "source_table": "summary_finance",
        "query_sql": (
            "SELECT ROUND(COALESCE(SUM(discount_amount), 0) * 100.0 / "
            "NULLIF(SUM(gross_sales), 0), 4) AS metric_value "
            "FROM summary_finance"
        ),
        "threshold_pct": 0.0,
        "description": "Discount amount as a percentage of gross sales",
    },
    {
        "metric_id": "top_region_share_pct",
        "metric_name": "Top region sales share",
        "source_table": "summary_region_sales",
        "query_sql": (
            "SELECT ROUND(COALESCE(MAX(region_total), 0) * 100.0 / "
            "NULLIF(SUM(region_total), 0), 4) AS metric_value FROM ("
            "SELECT region, SUM(net_sales) AS region_total "
            "FROM summary_region_sales GROUP BY region)"
        ),
        "threshold_pct": 0.0,
        "description": "Largest region share of net sales",
    },
]


def _task(task_name: str, output_table: str, description: str, sql: str) -> dict:
    return {
        "task_name": task_name,
        "output_table": output_table,
        "description": description,
        "sql": sql.strip() + "\n",
    }


TASKS = [
    _task(
        "raw_customers_validate",
        "raw_customers",
        "Validate customer source fields",
        """
        UPDATE raw_customers
           SET customer_name = TRIM(customer_name)
         WHERE length(customer_name) < 0;
        """,
    ),
    _task(
        "raw_products_validate",
        "raw_products",
        "Validate product source fields",
        """
        UPDATE raw_products
           SET product_name = TRIM(product_name)
         WHERE length(product_name) < 0;
        """,
    ),
    _task(
        "raw_orders_validate",
        "raw_orders",
        "Validate order source fields",
        """
        UPDATE raw_orders
           SET channel = UPPER(channel)
         WHERE length(channel) < 0;
        """,
    ),
    _task(
        "raw_order_items_validate",
        "raw_order_items",
        "Validate order line source fields",
        """
        UPDATE raw_order_items
           SET coupon_code = UPPER(coupon_code)
         WHERE length(coupon_code) < 0;
        """,
    ),
    _task(
        "detail_order_items_build",
        "detail_order_items",
        "Build conformed order line detail",
        """
        DELETE FROM detail_order_items;
        INSERT INTO detail_order_items (
            order_id, line_no, product_id, quantity, unit_price,
            gross_amount, discount_amount, tax_amount, net_amount,
            category, coupon_code
        )
        SELECT
            i.order_id,
            i.line_no,
            i.product_id,
            i.quantity,
            ROUND(i.unit_price, 2),
            ROUND(i.quantity * i.unit_price, 2),
            ROUND(i.discount_amount, 2),
            ROUND(i.tax_amount, 2),
            ROUND(i.quantity * i.unit_price - i.discount_amount +
                  i.tax_amount, 2),
            p.category,
            COALESCE(i.coupon_code, 'NONE')
        FROM raw_order_items AS i
        JOIN raw_products AS p ON p.product_id = i.product_id;
        """,
    ),
    _task(
        "detail_order_items_gross_refresh",
        "detail_order_items",
        "Refresh line gross amount",
        """
        UPDATE detail_order_items
           SET gross_amount = ROUND(quantity * unit_price, 2);
        """,
    ),
    _task(
        "detail_order_items_discount_guard",
        "detail_order_items",
        "Guard against negative discounts",
        """
        UPDATE detail_order_items
           SET discount_amount = ROUND(MAX(discount_amount, 0), 2);
        """,
    ),
    _task(
        "detail_order_items_net_refresh",
        "detail_order_items",
        "Refresh line net amount",
        """
        UPDATE detail_order_items
           SET net_amount = ROUND(gross_amount - discount_amount +
                                  tax_amount, 2);
        """,
    ),
    _task(
        "detail_order_items_coupon_normalize",
        "detail_order_items",
        "Normalize coupon codes",
        """
        UPDATE detail_order_items
           SET coupon_code = COALESCE(NULLIF(TRIM(coupon_code), ''), 'NONE');
        """,
    ),
    _task(
        "detail_orders_build",
        "detail_orders",
        "Build conformed order detail",
        """
        DELETE FROM detail_orders;
        INSERT INTO detail_orders (
            order_id, customer_id, order_date, channel, status, item_count,
            gross_amount, discount_amount, tax_amount, net_amount,
            customer_region, customer_segment
        )
        SELECT
            o.order_id,
            o.customer_id,
            substr(o.order_ts, 1, 10),
            o.channel,
            o.status,
            COUNT(i.line_no),
            ROUND(COALESCE(SUM(i.gross_amount), 0), 2),
            ROUND(COALESCE(SUM(i.discount_amount), 0), 2),
            ROUND(COALESCE(SUM(i.tax_amount), 0), 2),
            ROUND(COALESCE(SUM(i.net_amount), 0), 2),
            c.region,
            c.segment
        FROM raw_orders AS o
        JOIN raw_customers AS c ON c.customer_id = o.customer_id
        LEFT JOIN detail_order_items AS i ON i.order_id = o.order_id
        GROUP BY
            o.order_id, o.customer_id, substr(o.order_ts, 1, 10),
            o.channel, o.status, c.region, c.segment;
        """,
    ),
    _task(
        "detail_orders_item_count_refresh",
        "detail_orders",
        "Refresh order line quantity",
        """
        UPDATE detail_orders
           SET item_count = (
               SELECT COALESCE(SUM(i.quantity), 0)
                 FROM detail_order_items AS i
                WHERE i.order_id = detail_orders.order_id
           );
        """,
    ),
    _task(
        "detail_orders_amounts_refresh",
        "detail_orders",
        "Refresh order amounts",
        """
        UPDATE detail_orders
           SET gross_amount = (
                   SELECT ROUND(COALESCE(SUM(i.gross_amount), 0), 2)
                     FROM detail_order_items AS i
                    WHERE i.order_id = detail_orders.order_id
               ),
               discount_amount = (
                   SELECT ROUND(COALESCE(SUM(i.discount_amount), 0), 2)
                     FROM detail_order_items AS i
                    WHERE i.order_id = detail_orders.order_id
               ),
               tax_amount = (
                   SELECT ROUND(COALESCE(SUM(i.tax_amount), 0), 2)
                     FROM detail_order_items AS i
                    WHERE i.order_id = detail_orders.order_id
               );
        """,
    ),
    _task(
        "detail_orders_net_refresh",
        "detail_orders",
        "Refresh order net amount",
        """
        UPDATE detail_orders
           SET net_amount = ROUND(gross_amount - discount_amount +
                                  tax_amount, 2);
        """,
    ),
    _task(
        "detail_orders_customer_dim_refresh",
        "detail_orders",
        "Refresh customer region and segment",
        """
        UPDATE detail_orders
           SET customer_region = (
                   SELECT c.region
                     FROM raw_customers AS c
                    WHERE c.customer_id = detail_orders.customer_id
               ),
               customer_segment = (
                   SELECT c.segment
                     FROM raw_customers AS c
                    WHERE c.customer_id = detail_orders.customer_id
               );
        """,
    ),
    _task(
        "summary_daily_sales_build",
        "summary_daily_sales",
        "Build daily sales aggregate",
        """
        DELETE FROM summary_daily_sales;
        INSERT INTO summary_daily_sales (
            order_date, orders, units, gross_sales, net_sales, tax_amount
        )
        SELECT
            d.order_date,
            COUNT(DISTINCT d.order_id),
            COALESCE(SUM(i.quantity), 0),
            ROUND(COALESCE(SUM(i.gross_amount), 0), 2),
            ROUND(COALESCE(SUM(i.net_amount), 0), 2),
            ROUND(COALESCE(SUM(i.tax_amount), 0), 2)
        FROM detail_orders AS d
        JOIN detail_order_items AS i ON i.order_id = d.order_id
        WHERE d.status <> 'CANCELLED'
        GROUP BY d.order_date;
        """,
    ),
    _task(
        "summary_daily_sales_round",
        "summary_daily_sales",
        "Round daily sales amounts",
        """
        UPDATE summary_daily_sales
           SET gross_sales = ROUND(gross_sales, 2),
               net_sales = ROUND(net_sales, 2),
               tax_amount = ROUND(tax_amount, 2);
        """,
    ),
    _task(
        "summary_daily_sales_floor",
        "summary_daily_sales",
        "Floor daily net sales",
        """
        UPDATE summary_daily_sales
           SET net_sales = MAX(net_sales, 0);
        """,
    ),
    _task(
        "summary_daily_sales_units_refresh",
        "summary_daily_sales",
        "Refresh daily units",
        """
        UPDATE summary_daily_sales
           SET units = (
               SELECT COALESCE(SUM(i.quantity), 0)
                 FROM detail_order_items AS i
                 JOIN detail_orders AS d ON d.order_id = i.order_id
                WHERE d.order_date = summary_daily_sales.order_date
                  AND d.status <> 'CANCELLED'
           );
        """,
    ),
    _task(
        "summary_daily_sales_publish",
        "summary_daily_sales",
        "Publish daily sales aggregate",
        """
        UPDATE summary_daily_sales
           SET orders = MAX(orders, 0);
        """,
    ),
    _task(
        "summary_customer_sales_build",
        "summary_customer_sales",
        "Build customer sales aggregate",
        """
        DELETE FROM summary_customer_sales;
        INSERT INTO summary_customer_sales (
            customer_id, region, segment, orders, net_sales
        )
        SELECT
            d.customer_id,
            MAX(d.customer_region),
            MAX(d.customer_segment),
            COUNT(DISTINCT d.order_id),
            ROUND(COALESCE(SUM(d.net_amount), 0), 2)
        FROM detail_orders AS d
        WHERE d.status <> 'CANCELLED'
        GROUP BY d.customer_id;
        """,
    ),
    _task(
        "summary_customer_sales_round",
        "summary_customer_sales",
        "Round customer sales",
        """
        UPDATE summary_customer_sales
           SET net_sales = ROUND(net_sales, 2);
        """,
    ),
    _task(
        "summary_customer_sales_orders_refresh",
        "summary_customer_sales",
        "Refresh customer order counts",
        """
        UPDATE summary_customer_sales
           SET orders = (
               SELECT COUNT(*)
                 FROM detail_orders AS d
                WHERE d.customer_id = summary_customer_sales.customer_id
                  AND d.status <> 'CANCELLED'
           );
        """,
    ),
    _task(
        "summary_customer_sales_region_refresh",
        "summary_customer_sales",
        "Refresh customer region",
        """
        UPDATE summary_customer_sales
           SET region = COALESCE((
                   SELECT d.customer_region
                     FROM detail_orders AS d
                    WHERE d.customer_id = summary_customer_sales.customer_id
                    LIMIT 1
               ), 'UNKNOWN');
        """,
    ),
    _task(
        "summary_customer_sales_publish",
        "summary_customer_sales",
        "Publish customer sales aggregate",
        """
        UPDATE summary_customer_sales
           SET segment = COALESCE(segment, 'UNKNOWN');
        """,
    ),
    _task(
        "summary_product_sales_build",
        "summary_product_sales",
        "Build product sales aggregate",
        """
        DELETE FROM summary_product_sales;
        INSERT INTO summary_product_sales (
            product_id, category, units, gross_sales, net_sales
        )
        SELECT
            i.product_id,
            MAX(i.category),
            COALESCE(SUM(i.quantity), 0),
            ROUND(COALESCE(SUM(i.gross_amount), 0), 2),
            ROUND(COALESCE(SUM(i.net_amount), 0), 2)
        FROM detail_order_items AS i
        JOIN detail_orders AS d ON d.order_id = i.order_id
        WHERE d.status <> 'CANCELLED'
        GROUP BY i.product_id;
        """,
    ),
    _task(
        "summary_product_sales_round",
        "summary_product_sales",
        "Round product sales",
        """
        UPDATE summary_product_sales
           SET gross_sales = ROUND(gross_sales, 2),
               net_sales = ROUND(net_sales, 2);
        """,
    ),
    _task(
        "summary_product_sales_units_refresh",
        "summary_product_sales",
        "Refresh product units",
        """
        UPDATE summary_product_sales
           SET units = (
               SELECT COALESCE(SUM(i.quantity), 0)
                 FROM detail_order_items AS i
                 JOIN detail_orders AS d ON d.order_id = i.order_id
                WHERE i.product_id = summary_product_sales.product_id
                  AND d.status <> 'CANCELLED'
           );
        """,
    ),
    _task(
        "summary_product_sales_category_refresh",
        "summary_product_sales",
        "Refresh product category",
        """
        UPDATE summary_product_sales
           SET category = (
               SELECT p.category
                 FROM raw_products AS p
                WHERE p.product_id = summary_product_sales.product_id
           );
        """,
    ),
    _task(
        "summary_product_sales_publish",
        "summary_product_sales",
        "Publish product sales aggregate",
        """
        UPDATE summary_product_sales
           SET net_sales = MAX(net_sales, 0);
        """,
    ),
    _task(
        "summary_channel_sales_build",
        "summary_channel_sales",
        "Build channel sales aggregate",
        """
        DELETE FROM summary_channel_sales;
        INSERT INTO summary_channel_sales (channel, orders, net_sales)
        SELECT
            d.channel,
            COUNT(DISTINCT d.order_id),
            ROUND(COALESCE(SUM(d.net_amount), 0), 2)
        FROM detail_orders AS d
        WHERE d.status <> 'CANCELLED'
        GROUP BY d.channel;
        """,
    ),
    _task(
        "summary_channel_sales_round",
        "summary_channel_sales",
        "Round channel sales",
        """
        UPDATE summary_channel_sales
           SET net_sales = ROUND(net_sales, 2);
        """,
    ),
    _task(
        "summary_channel_sales_orders_refresh",
        "summary_channel_sales",
        "Refresh channel order counts",
        """
        UPDATE summary_channel_sales
           SET orders = (
               SELECT COUNT(DISTINCT d.order_id)
                 FROM detail_orders AS d
                WHERE d.channel = summary_channel_sales.channel
                  AND d.status <> 'CANCELLED'
           );
        """,
    ),
    _task(
        "summary_channel_sales_publish",
        "summary_channel_sales",
        "Publish channel sales aggregate",
        """
        UPDATE summary_channel_sales
           SET net_sales = MAX(net_sales, 0);
        """,
    ),
    _task(
        "summary_region_sales_build",
        "summary_region_sales",
        "Build regional sales aggregate",
        """
        DELETE FROM summary_region_sales;
        INSERT INTO summary_region_sales (
            region, segment, orders, net_sales
        )
        SELECT
            d.customer_region,
            d.customer_segment,
            COUNT(DISTINCT d.order_id),
            ROUND(COALESCE(SUM(d.net_amount), 0), 2)
        FROM detail_orders AS d
        WHERE d.status <> 'CANCELLED'
        GROUP BY d.customer_region, d.customer_segment;
        """,
    ),
    _task(
        "summary_region_sales_round",
        "summary_region_sales",
        "Round regional sales",
        """
        UPDATE summary_region_sales
           SET net_sales = ROUND(net_sales, 2);
        """,
    ),
    _task(
        "summary_region_sales_orders_refresh",
        "summary_region_sales",
        "Refresh regional order counts",
        """
        UPDATE summary_region_sales
           SET orders = (
               SELECT COUNT(DISTINCT d.order_id)
                 FROM detail_orders AS d
                WHERE d.customer_region = summary_region_sales.region
                  AND d.customer_segment = summary_region_sales.segment
                  AND d.status <> 'CANCELLED'
           );
        """,
    ),
    _task(
        "summary_region_sales_publish",
        "summary_region_sales",
        "Publish regional sales aggregate",
        """
        UPDATE summary_region_sales
           SET net_sales = MAX(net_sales, 0);
        """,
    ),
    _task(
        "summary_finance_build",
        "summary_finance",
        "Build finance aggregate",
        """
        DELETE FROM summary_finance;
        INSERT INTO summary_finance (
            currency, gross_sales, net_sales, tax_amount, discount_amount
        )
        SELECT
            o.currency,
            ROUND(COALESCE(SUM(i.gross_amount), 0), 2),
            ROUND(COALESCE(SUM(i.net_amount), 0), 2),
            ROUND(COALESCE(SUM(i.tax_amount), 0), 2),
            ROUND(COALESCE(SUM(i.discount_amount), 0), 2)
        FROM raw_orders AS o
        JOIN detail_orders AS d ON d.order_id = o.order_id
        JOIN detail_order_items AS i ON i.order_id = d.order_id
        WHERE d.status <> 'CANCELLED'
        GROUP BY o.currency;
        """,
    ),
    _task(
        "summary_finance_round",
        "summary_finance",
        "Round finance amounts",
        """
        UPDATE summary_finance
           SET gross_sales = ROUND(gross_sales, 2),
               net_sales = ROUND(net_sales, 2),
               tax_amount = ROUND(tax_amount, 2),
               discount_amount = ROUND(discount_amount, 2);
        """,
    ),
    _task(
        "summary_finance_publish",
        "summary_finance",
        "Publish finance aggregate",
        """
        UPDATE summary_finance
           SET net_sales = ROUND(gross_sales - discount_amount +
                                 tax_amount, 2);
        """,
    ),
]


if len(TASKS) != 40:
    raise AssertionError(f"expected 40 tasks, got {len(TASKS)}")
if len(TABLE_SCHEMA) != 12:
    raise AssertionError(f"expected 12 tables, got {len(TABLE_SCHEMA)}")


def quote_identifier(name: str) -> str:
    return '"' + name.replace('"', '""') + '"'
