/*
 * Deterministic initialization for the index-cutover gateway validation.
 * Idempotent: re-running the file drops and recreates every object.
 *
 * Dataset shape (fixed by deterministic formulas, no RAND/NEWID):
 *   30,000 orders
 *   12 tenants      TenantId  = seq % 12        -> 2,500 rows each
 *   5 statuses      Status    = seq % 5         -> 6,000 rows each
 *   3 years         UpdatedAt = base + (seq % 1095) days
 * Every (TenantId, Status) pair holds exactly 500 rows.
 */
SET NOCOUNT ON;
SET XACT_ABORT ON;

/* The bootstrapper runs the drop/create block against master, then switches
   its own connection to $(DatabaseName) before the object DDL below. */
IF DB_ID(N'$(DatabaseName)') IS NOT NULL
BEGIN
    ALTER DATABASE [$(DatabaseName)] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    ALTER DATABASE [$(DatabaseName)] SET MULTI_USER;
    DROP DATABASE [$(DatabaseName)];
END
GO

CREATE DATABASE [$(DatabaseName)];
GO

/* Snapshot isolation lets each pagination walk read a stable, point-in-time
   visible set even while the same order batch is being updated. */
ALTER DATABASE [$(DatabaseName)] SET ALLOW_SNAPSHOT_ISOLATION ON;
ALTER DATABASE [$(DatabaseName)] SET READ_COMMITTED_SNAPSHOT OFF;
GO

USE [$(DatabaseName)];
GO

CREATE TABLE dbo.Orders
(
    OrderId    bigint        NOT NULL CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED,
    TenantId   int           NOT NULL,
    Status     tinyint       NOT NULL,
    Amount     decimal(18,2) NOT NULL,
    UpdatedAt  datetime2(3)  NOT NULL,
    CreatedAt  datetime2(3)  NOT NULL,
    RowVersion rowversion    NOT NULL,
    CONSTRAINT CK_Orders_Status CHECK (Status BETWEEN 0 AND 4)
);

/* Legacy access path: old services filter by Status and page by OrderId. */
CREATE NONCLUSTERED INDEX IX_Orders_Status
    ON dbo.Orders (Status, OrderId)
    INCLUDE (TenantId, Amount, UpdatedAt);

/* New covering index from the change ticket. */
CREATE NONCLUSTERED INDEX IX_Orders_TenantStatus
    ON dbo.Orders (TenantId, Status)
    INCLUDE (Amount, UpdatedAt);

/* Two simulated legacy service instances. A heartbeat older than the
   configured expiry means the instance is gone and the old index may retire. */
CREATE TABLE dbo.LegacyHeartbeat
(
    InstanceId   int           NOT NULL CONSTRAINT PK_LegacyHeartbeat PRIMARY KEY,
    InstanceName nvarchar(64)  NOT NULL,
    LastBeatUtc  datetime2(3)  NOT NULL
);

INSERT dbo.LegacyHeartbeat (InstanceId, InstanceName, LastBeatUtc)
VALUES
    (1, N'legacy-instance-a', SYSUTCDATETIME()),
    (2, N'legacy-instance-b', SYSUTCDATETIME());

/* Deterministic 30k-row generator built purely from a set-based tally table. */
;WITH E1(seq) AS
(
    SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
    UNION ALL SELECT 0 UNION ALL SELECT 0 UNION ALL SELECT 0
),
E2(seq) AS (SELECT 0 FROM E1 a CROSS JOIN E1 b),
E4(seq) AS (SELECT 0 FROM E2 a CROSS JOIN E2 b),
E5(seq) AS (SELECT 0 FROM E4 a CROSS JOIN E1 b),
Nums(n)  AS (SELECT ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) - 1 FROM E5)
INSERT dbo.Orders (OrderId, TenantId, Status, Amount, UpdatedAt, CreatedAt)
SELECT
    OrderId   = n + 1,
    TenantId  = n % 12,
    Status    = n % 5,
    Amount    = CAST(10 + (n * 7) % 9990 + (n % 100) / 100.0 AS decimal(18,2)),
    UpdatedAt = DATEADD(DAY, n % 1095, CAST('2023-01-01T00:00:00' AS datetime2(3))),
    CreatedAt = DATEADD(DAY, (n / 1095) % 365, CAST('2022-06-01T00:00:00' AS datetime2(3)))
FROM Nums
WHERE n < 30000;

/* Audit view used by every report: only logical index names, no host data. */
GO
CREATE OR ALTER VIEW dbo.vw_IndexCatalog
AS
SELECT
    i.name              AS IndexName,
    i.type_desc         AS IndexType,
    i.is_disabled       AS IsDisabled,
    STUFF((
        SELECT N', ' + c.name
        FROM sys.index_columns ic
        JOIN sys.columns c
          ON c.object_id = ic.object_id AND c.column_id = ic.column_id
        WHERE ic.object_id = i.object_id
          AND ic.index_id  = i.index_id
          AND ic.is_included_column = 0
        ORDER BY ic.key_ordinal
        FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 2, N'') AS KeyColumns,
    STUFF((
        SELECT N', ' + c.name
        FROM sys.index_columns ic
        JOIN sys.columns c
          ON c.object_id = ic.object_id AND c.column_id = ic.column_id
        WHERE ic.object_id = i.object_id
          AND ic.index_id  = i.index_id
          AND ic.is_included_column = 1
        ORDER BY c.name
        FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 2, N'') AS IncludeColumns
FROM sys.indexes i
WHERE i.object_id = OBJECT_ID(N'dbo.Orders')
  AND i.name IS NOT NULL;
