/* OrderRouter schema: hot table Orders, cold table OrdersArchive, order items.
   Idempotent: drops and recreates the tables. */
SET NOCOUNT ON;

IF OBJECT_ID(N'dbo.OrderItems', N'U') IS NOT NULL DROP TABLE dbo.OrderItems;
IF OBJECT_ID(N'dbo.Orders', N'U') IS NOT NULL DROP TABLE dbo.Orders;
IF OBJECT_ID(N'dbo.OrdersArchive', N'U') IS NOT NULL DROP TABLE dbo.OrdersArchive;

CREATE TABLE dbo.Orders
(
    OrderId     bigint          NOT NULL,
    OrderNo     nvarchar(32)    NOT NULL,
    CustomerId  int             NOT NULL,
    CreatedAt   datetime2(3)    NOT NULL,
    LastUpdated datetime2(3)    NOT NULL,
    Amount      decimal(18, 2)  NOT NULL,
    Currency    char(3)         NOT NULL,
    Status      nvarchar(16)    NOT NULL,
    CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED (OrderId)
);
CREATE UNIQUE INDEX UX_Orders_OrderNo ON dbo.Orders (OrderNo);
CREATE INDEX IX_Orders_CreatedAt ON dbo.Orders (CreatedAt);

CREATE TABLE dbo.OrdersArchive
(
    OrderId     bigint          NOT NULL,
    OrderNo     nvarchar(32)    NOT NULL,
    CustomerId  int             NOT NULL,
    CreatedAt   datetime2(3)    NOT NULL,
    LastUpdated datetime2(3)    NOT NULL,
    Amount      decimal(18, 2)  NOT NULL,
    Currency    char(3)         NOT NULL,
    Status      nvarchar(16)    NOT NULL,
    CONSTRAINT PK_OrdersArchive PRIMARY KEY CLUSTERED (OrderId)
);
CREATE UNIQUE INDEX UX_OrdersArchive_OrderNo ON dbo.OrdersArchive (OrderNo);
CREATE INDEX IX_OrdersArchive_CreatedAt ON dbo.OrdersArchive (CreatedAt);

CREATE TABLE dbo.OrderItems
(
    ItemId   bigint         NOT NULL,
    OrderId  bigint         NOT NULL,
    Sku      nvarchar(32)   NOT NULL,
    Quantity int            NOT NULL,
    Price    decimal(18, 2) NOT NULL,
    CONSTRAINT PK_OrderItems PRIMARY KEY CLUSTERED (ItemId)
);
CREATE INDEX IX_OrderItems_OrderId ON dbo.OrderItems (OrderId);
