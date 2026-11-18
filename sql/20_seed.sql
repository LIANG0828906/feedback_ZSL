SET NOCOUNT ON;
SET XACT_ABORT ON;

;WITH Numbers AS
(
    SELECT 1 AS n
    UNION ALL
    SELECT n + 1 FROM Numbers WHERE n < 20000
)
INSERT INTO dbo.OrderHeader
(
    TenantId, OrderId, OrderNo, CustomerName, Status,
    OrderDate, Amount, Currency, ShipAddress, UpdatedAt
)
SELECT
    1 + ((n - 1) % 4) AS TenantId,
    100000 + n AS OrderId,
    CONCAT('ORD-', RIGHT('0000000' + CONVERT(varchar(10), n), 7)) AS OrderNo,
    CASE
        WHEN n % 37 = 0 THEN NULL
        ELSE CONCAT('Customer-', RIGHT('00000' + CONVERT(varchar(10), (n * 17) % 100000), 5))
    END AS CustomerName,
    CHOOSE(1 + (((n - 1) / 4) % 4), 'PAID', 'NEW', 'SHIPPED', 'CANCELLED') AS Status,
    DATEADD(second, (n * 7919) % 31536000, CONVERT(datetime2(0), '2025-01-01T00:00:00')) AS OrderDate,
    CAST((n * 37) % 100000 AS decimal(18,2)) / 100.0 + 5.00 AS Amount,
    'CNY' AS Currency,
    CASE
        WHEN n % 11 = 0 THEN NULL
        ELSE CONCAT('Road ', n % 200, ', Zone ', n % 17)
    END AS ShipAddress,
    DATEADD(second, n % 3600, DATEADD(second, (n * 7919) % 31536000, CONVERT(datetime2(0), '2025-01-01T00:00:00'))) AS UpdatedAt
FROM Numbers
OPTION (MAXRECURSION 20000);

;WITH LineNums AS
(
    SELECT 1 AS LineNum
    UNION ALL
    SELECT 2
)
INSERT INTO dbo.OrderLine
(
    TenantId, OrderLineId, OrderId, Status, LineNum, Sku,
    Quantity, UnitPrice, LineAmount, ProductNote, UpdatedAt
)
SELECT
    h.TenantId,
    h.OrderId * 10 + l.LineNum AS OrderLineId,
    h.OrderId,
    h.Status,
    l.LineNum,
    CASE
        WHEN (h.OrderId + l.LineNum) % 13 = 0 THEN 'SKU-X'
        ELSE RIGHT('000000' + CONVERT(varchar(10), 1000 + ((h.OrderId + l.LineNum) % 6)), 6)
    END AS Sku,
    1 + ((h.OrderId + l.LineNum) % 4) AS Quantity,
    CAST(5 + ((h.OrderId + l.LineNum) % 900) / 100.0 AS decimal(18,2)) AS UnitPrice,
    CAST((1 + ((h.OrderId + l.LineNum) % 4)) * (5 + ((h.OrderId + l.LineNum) % 900) / 100.0) AS decimal(18,2)) AS LineAmount,
    CASE
        WHEN (h.OrderId + l.LineNum) % 17 = 0 THEN NULL
        ELSE CONCAT('Line note ', (h.OrderId + l.LineNum) % 97)
    END AS ProductNote,
    DATEADD(second, (h.OrderId + l.LineNum) % 600, h.UpdatedAt) AS UpdatedAt
FROM dbo.OrderHeader AS h
CROSS JOIN LineNums AS l
WHERE h.OrderId <= 110000;

INSERT INTO dbo.OrderSummaryDaily
(
    TenantId, Status, SummaryDate, OrderCount,
    TotalAmount, Currency, SourceWatermark
)
SELECT
    h.TenantId,
    h.Status,
    CAST(h.OrderDate AS date) AS SummaryDate,
    COUNT_BIG(*) AS OrderCount,
    SUM(h.Amount) AS TotalAmount,
    MIN(h.Currency) AS Currency,
    MAX(h.UpdatedAt) AS SourceWatermark
FROM dbo.OrderHeader AS h
GROUP BY h.TenantId, h.Status, CAST(h.OrderDate AS date);
