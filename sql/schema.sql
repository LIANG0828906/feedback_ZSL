-- 仓储结算链路：出库单（父表）与结算明细（子表）
-- 主库(55432)与备库(55433)使用完全相同的结构
CREATE SCHEMA IF NOT EXISTS wms;

CREATE TABLE IF NOT EXISTS wms.outbound_order (
    order_id      bigserial PRIMARY KEY,
    order_no      text        NOT NULL UNIQUE,
    warehouse     text        NOT NULL,
    status        text        NOT NULL,
    total_amount  numeric(14,2) NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wms.settlement_detail (
    detail_id   bigserial PRIMARY KEY,
    order_id    bigint       NOT NULL REFERENCES wms.outbound_order(order_id),
    item_name   text         NOT NULL,
    quantity    integer      NOT NULL,
    unit_price  numeric(12,2) NOT NULL,
    amount      numeric(14,2) NOT NULL,
    status      text         NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_settlement_order ON wms.settlement_detail(order_id);

-- 逻辑复制需要完整副本标识才能正确回放 UPDATE/DELETE
ALTER TABLE wms.outbound_order    REPLICA IDENTITY FULL;
ALTER TABLE wms.settlement_detail REPLICA IDENTITY FULL;
