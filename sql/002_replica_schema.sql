SET NOCOUNT ON;
SET XACT_ABORT ON;

IF DB_ID(N'$(ReplicaDatabase)') IS NULL
BEGIN
    DECLARE @create_database nvarchar(max) =
        N'CREATE DATABASE ' + QUOTENAME(N'$(ReplicaDatabase)') + N';';
    EXEC sys.sp_executesql @create_database;
END;
GO

USE [$(ReplicaDatabase)];
GO

SET NOCOUNT ON;
SET XACT_ABORT ON;

IF OBJECT_ID(N'dbo.Orders', N'U') IS NULL
BEGIN
    CREATE TABLE dbo.Orders
    (
        OrderNo             varchar(32)    NOT NULL,
        TenantId            varchar(32)    NOT NULL,
        Status              varchar(16)    NOT NULL,
        Amount              decimal(18, 2) NOT NULL,
        ItemCount           int            NOT NULL,
        CreatedAtUtc        datetime2(3)   NOT NULL,
        UpdatedAtUtc        datetime2(3)   NOT NULL,
        LastWriteWatermark  bigint         NOT NULL,
        CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED (OrderNo),
        CONSTRAINT CK_Orders_Status
            CHECK (Status IN ('PENDING', 'CONFIRMED', 'CANCELLED')),
        CONSTRAINT CK_Orders_ItemCount CHECK (ItemCount > 0),
        CONSTRAINT CK_Orders_Amount CHECK (Amount >= 0)
    );
END;
GO

IF OBJECT_ID(N'dbo.OrderLines', N'U') IS NULL
BEGIN
    CREATE TABLE dbo.OrderLines
    (
        OrderNo         varchar(32)    NOT NULL,
        LineNo          int            NOT NULL,
        TenantId        varchar(32)    NOT NULL,
        Sku             varchar(64)    NOT NULL,
        Quantity        int            NOT NULL,
        UnitPrice       decimal(18, 2) NOT NULL,
        LineAmount      decimal(18, 2) NOT NULL,
        WriteWatermark  bigint         NOT NULL,
        CONSTRAINT PK_OrderLines PRIMARY KEY CLUSTERED (OrderNo, LineNo),
        CONSTRAINT FK_OrderLines_Orders
            FOREIGN KEY (OrderNo) REFERENCES dbo.Orders (OrderNo),
        CONSTRAINT CK_OrderLines_Quantity CHECK (Quantity > 0),
        CONSTRAINT CK_OrderLines_Prices
            CHECK (UnitPrice >= 0 AND LineAmount >= 0)
    );
END;
GO

IF OBJECT_ID(N'dbo.ConsistencyWatermark', N'U') IS NULL
BEGIN
    CREATE TABLE dbo.ConsistencyWatermark
    (
        Name              varchar(64)  NOT NULL,
        AppliedWatermark  bigint       NOT NULL,
        UpdatedAtUtc      datetime2(3) NOT NULL,
        CONSTRAINT PK_ConsistencyWatermark PRIMARY KEY CLUSTERED (Name)
    );
END;
GO

IF OBJECT_ID(N'dbo.TopologyProbe', N'U') IS NULL
BEGIN
    CREATE TABLE dbo.TopologyProbe
    (
        ProbeId       int          NOT NULL,
        TouchedAtUtc  datetime2(3) NOT NULL,
        CONSTRAINT PK_TopologyProbe PRIMARY KEY CLUSTERED (ProbeId)
    );
END;
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE object_id = OBJECT_ID(N'dbo.Orders')
      AND name = N'IX_Orders_Tenant_Status_OrderNo'
)
BEGIN
    CREATE INDEX IX_Orders_Tenant_Status_OrderNo
        ON dbo.Orders (TenantId, Status, OrderNo)
        INCLUDE (Amount, ItemCount, CreatedAtUtc, UpdatedAtUtc, LastWriteWatermark);
END;
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE object_id = OBJECT_ID(N'dbo.Orders')
      AND name = N'IX_Orders_LastWriteWatermark'
)
BEGIN
    CREATE INDEX IX_Orders_LastWriteWatermark
        ON dbo.Orders (LastWriteWatermark);
END;
GO

IF NOT EXISTS (SELECT 1 FROM dbo.ConsistencyWatermark WHERE Name = N'orders')
BEGIN
    INSERT INTO dbo.ConsistencyWatermark (Name, AppliedWatermark, UpdatedAtUtc)
    VALUES (N'orders', 0, SYSUTCDATETIME());
END;

IF NOT EXISTS (SELECT 1 FROM dbo.TopologyProbe WHERE ProbeId = 1)
BEGIN
    INSERT INTO dbo.TopologyProbe (ProbeId, TouchedAtUtc)
    VALUES (1, SYSUTCDATETIME());
END;
GO

USE [master];
GO

ALTER DATABASE [$(ReplicaDatabase)] SET READ_ONLY WITH ROLLBACK IMMEDIATE;
GO

SELECT
    DB_NAME() AS database_name,
    DATABASEPROPERTYEX(N'$(ReplicaDatabase)', 'Updateability') AS updateability;
GO

