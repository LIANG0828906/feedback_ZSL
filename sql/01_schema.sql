SET NOCOUNT ON;
SET XACT_ABORT ON;

IF OBJECT_ID(N'dbo.WriteReceipts', N'U') IS NOT NULL DROP TABLE dbo.WriteReceipts;
IF OBJECT_ID(N'dbo.OrderItems', N'U') IS NOT NULL DROP TABLE dbo.OrderItems;
IF OBJECT_ID(N'dbo.Orders', N'U') IS NOT NULL DROP TABLE dbo.Orders;

CREATE TABLE dbo.Orders
(
    OrderId    bigint         NOT NULL CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED,
    TenantId   int            NOT NULL,
    Status     tinyint        NOT NULL,
    Amount     decimal(18,2)  NOT NULL,
    Currency   char(3)        NOT NULL,
    UpdatedAt  datetime2(3)   NOT NULL,
    CreatedAt  datetime2(3)   NOT NULL,
    Revision   bigint         NOT NULL,
    CONSTRAINT CK_Orders_Status CHECK (Status BETWEEN 0 AND 4),
    CONSTRAINT CK_Orders_Currency CHECK (Currency IN ('CNY', 'USD', 'EUR')),
    CONSTRAINT CK_Orders_Revision CHECK (Revision > 0)
);

CREATE NONCLUSTERED INDEX IX_Orders_TenantStatus
    ON dbo.Orders (TenantId, Status, UpdatedAt, OrderId)
    INCLUDE (Amount, Currency, Revision);

CREATE TABLE dbo.OrderItems
(
    OrderId    bigint         NOT NULL,
    LineNumber int            NOT NULL,
    Sku        nvarchar(40)   NOT NULL,
    Quantity   int            NOT NULL,
    UnitAmount decimal(18,2)  NOT NULL,
    CONSTRAINT PK_OrderItems PRIMARY KEY CLUSTERED (OrderId, LineNumber),
    CONSTRAINT FK_OrderItems_Orders FOREIGN KEY (OrderId)
        REFERENCES dbo.Orders (OrderId) ON DELETE CASCADE,
    CONSTRAINT CK_OrderItems_Quantity CHECK (Quantity > 0)
);

CREATE NONCLUSTERED INDEX IX_OrderItems_OrderId
    ON dbo.OrderItems (OrderId, LineNumber);

CREATE TABLE dbo.WriteReceipts
(
    WriteId     nvarchar(96)   NOT NULL CONSTRAINT PK_WriteReceipts PRIMARY KEY,
    OrderId     bigint         NOT NULL,
    TenantId    int            NOT NULL,
    Revision    bigint         NOT NULL,
    Kind        nvarchar(24)   NOT NULL,
    PayloadHash char(64)       NOT NULL,
    CommittedAt datetime2(3)   NOT NULL
);

CREATE NONCLUSTERED INDEX IX_WriteReceipts_OrderId
    ON dbo.WriteReceipts (OrderId, CommittedAt)
    INCLUDE (WriteId, Revision, Kind, PayloadHash);

