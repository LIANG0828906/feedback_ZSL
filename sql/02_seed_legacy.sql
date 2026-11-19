SET NOCOUNT ON;
SET XACT_ABORT ON;

DELETE FROM dbo.WriteReceipts;
DELETE FROM dbo.OrderItems;
DELETE FROM dbo.Orders;
GO

;WITH E1(n) AS
(
    SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
),
E2(n) AS (SELECT 0 FROM E1 a CROSS JOIN E1 b),
E4(n) AS (SELECT 0 FROM E2 a CROSS JOIN E2 b),
E5(n) AS (SELECT 0 FROM E4 a CROSS JOIN E1 b),
Nums(n) AS (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 FROM E5)
INSERT dbo.Orders
    (OrderId, TenantId, Status, Amount, Currency, UpdatedAt, CreatedAt, Revision)
SELECT
    OrderId   = n + 1,
    TenantId  = (n * 7 + 11) % 16,
    Status    = n % 5,
    Amount    = CAST((1000 + ((n * 37 + 20261119) % 900000)) / 100.0 AS decimal(18,2)),
    Currency  = CHOOSE(1 + n % 3, 'CNY', 'USD', 'EUR'),
    UpdatedAt = DATEADD(SECOND, n * 7, CAST('2026-01-01T00:00:00' AS datetime2(3))),
    CreatedAt = DATEADD(SECOND, n * 5, CAST('2025-06-01T00:00:00' AS datetime2(3))),
    Revision  = 1
FROM Nums
WHERE n >= 0 AND n < 5000
OPTION (MAXDOP 1);
GO

;WITH E1(n) AS
(
    SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
),
E2(n) AS (SELECT 0 FROM E1 a CROSS JOIN E1 b),
E4(n) AS (SELECT 0 FROM E2 a CROSS JOIN E2 b),
E5(n) AS (SELECT 0 FROM E4 a CROSS JOIN E1 b),
Nums(n) AS (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 FROM E5)
INSERT dbo.Orders
    (OrderId, TenantId, Status, Amount, Currency, UpdatedAt, CreatedAt, Revision)
SELECT
    OrderId   = n + 1,
    TenantId  = (n * 7 + 11) % 16,
    Status    = n % 5,
    Amount    = CAST((1000 + ((n * 37 + 20261119) % 900000)) / 100.0 AS decimal(18,2)),
    Currency  = CHOOSE(1 + n % 3, 'CNY', 'USD', 'EUR'),
    UpdatedAt = DATEADD(SECOND, n * 7, CAST('2026-01-01T00:00:00' AS datetime2(3))),
    CreatedAt = DATEADD(SECOND, n * 5, CAST('2025-06-01T00:00:00' AS datetime2(3))),
    Revision  = 1
FROM Nums
WHERE n >= 5000 AND n < 10000
OPTION (MAXDOP 1);
GO

;WITH E1(n) AS
(
    SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
),
E2(n) AS (SELECT 0 FROM E1 a CROSS JOIN E1 b),
E4(n) AS (SELECT 0 FROM E2 a CROSS JOIN E2 b),
E5(n) AS (SELECT 0 FROM E4 a CROSS JOIN E1 b),
Nums(n) AS (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 FROM E5)
INSERT dbo.Orders
    (OrderId, TenantId, Status, Amount, Currency, UpdatedAt, CreatedAt, Revision)
SELECT
    OrderId   = n + 1,
    TenantId  = (n * 7 + 11) % 16,
    Status    = n % 5,
    Amount    = CAST((1000 + ((n * 37 + 20261119) % 900000)) / 100.0 AS decimal(18,2)),
    Currency  = CHOOSE(1 + n % 3, 'CNY', 'USD', 'EUR'),
    UpdatedAt = DATEADD(SECOND, n * 7, CAST('2026-01-01T00:00:00' AS datetime2(3))),
    CreatedAt = DATEADD(SECOND, n * 5, CAST('2025-06-01T00:00:00' AS datetime2(3))),
    Revision  = 1
FROM Nums
WHERE n >= 10000 AND n < 15000
OPTION (MAXDOP 1);
GO

;WITH E1(n) AS
(
    SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
),
E2(n) AS (SELECT 0 FROM E1 a CROSS JOIN E1 b),
E4(n) AS (SELECT 0 FROM E2 a CROSS JOIN E2 b),
E5(n) AS (SELECT 0 FROM E4 a CROSS JOIN E1 b),
Nums(n) AS (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 FROM E5)
INSERT dbo.Orders
    (OrderId, TenantId, Status, Amount, Currency, UpdatedAt, CreatedAt, Revision)
SELECT
    OrderId   = n + 1,
    TenantId  = (n * 7 + 11) % 16,
    Status    = n % 5,
    Amount    = CAST((1000 + ((n * 37 + 20261119) % 900000)) / 100.0 AS decimal(18,2)),
    Currency  = CHOOSE(1 + n % 3, 'CNY', 'USD', 'EUR'),
    UpdatedAt = DATEADD(SECOND, n * 7, CAST('2026-01-01T00:00:00' AS datetime2(3))),
    CreatedAt = DATEADD(SECOND, n * 5, CAST('2025-06-01T00:00:00' AS datetime2(3))),
    Revision  = 1
FROM Nums
WHERE n >= 15000 AND n < 20000
OPTION (MAXDOP 1);
GO

INSERT dbo.OrderItems (OrderId, LineNumber, Sku, Quantity, UnitAmount)
SELECT
    o.OrderId,
    l.LineNumber,
    CONCAT('SKU-', RIGHT(CONCAT('00000', o.OrderId % 997), 5), '-', l.LineNumber),
    1 + ((o.OrderId + l.LineNumber * 3) % 5),
    CAST(ROUND(o.Amount / (1 + (o.OrderId % 3)), 2) AS decimal(18,2))
FROM dbo.Orders o
CROSS JOIN (VALUES (1), (2), (3)) l(LineNumber)
WHERE o.OrderId <= 5000 AND l.LineNumber <= 1 + (o.OrderId % 3)
OPTION (MAXDOP 1);
GO

INSERT dbo.OrderItems (OrderId, LineNumber, Sku, Quantity, UnitAmount)
SELECT
    o.OrderId,
    l.LineNumber,
    CONCAT('SKU-', RIGHT(CONCAT('00000', o.OrderId % 997), 5), '-', l.LineNumber),
    1 + ((o.OrderId + l.LineNumber * 3) % 5),
    CAST(ROUND(o.Amount / (1 + (o.OrderId % 3)), 2) AS decimal(18,2))
FROM dbo.Orders o
CROSS JOIN (VALUES (1), (2), (3)) l(LineNumber)
WHERE o.OrderId > 5000 AND o.OrderId <= 10000
  AND l.LineNumber <= 1 + (o.OrderId % 3)
OPTION (MAXDOP 1);
GO

INSERT dbo.OrderItems (OrderId, LineNumber, Sku, Quantity, UnitAmount)
SELECT
    o.OrderId,
    l.LineNumber,
    CONCAT('SKU-', RIGHT(CONCAT('00000', o.OrderId % 997), 5), '-', l.LineNumber),
    1 + ((o.OrderId + l.LineNumber * 3) % 5),
    CAST(ROUND(o.Amount / (1 + (o.OrderId % 3)), 2) AS decimal(18,2))
FROM dbo.Orders o
CROSS JOIN (VALUES (1), (2), (3)) l(LineNumber)
WHERE o.OrderId > 10000 AND o.OrderId <= 15000
  AND l.LineNumber <= 1 + (o.OrderId % 3)
OPTION (MAXDOP 1);
GO

INSERT dbo.OrderItems (OrderId, LineNumber, Sku, Quantity, UnitAmount)
SELECT
    o.OrderId,
    l.LineNumber,
    CONCAT('SKU-', RIGHT(CONCAT('00000', o.OrderId % 997), 5), '-', l.LineNumber),
    1 + ((o.OrderId + l.LineNumber * 3) % 5),
    CAST(ROUND(o.Amount / (1 + (o.OrderId % 3)), 2) AS decimal(18,2))
FROM dbo.Orders o
CROSS JOIN (VALUES (1), (2), (3)) l(LineNumber)
WHERE o.OrderId > 15000
  AND l.LineNumber <= 1 + (o.OrderId % 3)
OPTION (MAXDOP 1);
GO

IF (SELECT COUNT_BIG(*) FROM dbo.Orders) <> 20000
    THROW 51000, 'seed expected exactly 20000 orders', 1;
