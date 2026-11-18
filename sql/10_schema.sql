SET NOCOUNT ON;
SET XACT_ABORT ON;

IF OBJECT_ID(N'dbo.OrderLine', N'U') IS NOT NULL
    DROP TABLE dbo.OrderLine;
IF OBJECT_ID(N'dbo.OrderHeader', N'U') IS NOT NULL
    DROP TABLE dbo.OrderHeader;
IF OBJECT_ID(N'dbo.OrderSummaryDaily', N'U') IS NOT NULL
    DROP TABLE dbo.OrderSummaryDaily;

CREATE TABLE dbo.OrderHeader
(
    TenantId       int            NOT NULL,
    OrderId        bigint         NOT NULL,
    OrderNo        varchar(32)    NOT NULL,
    CustomerName   nvarchar(100)  NULL,
    Status         varchar(16)    NOT NULL,
    OrderDate      datetime2(0)   NOT NULL,
    Amount         decimal(18,2)  NOT NULL,
    Currency       char(3)        NOT NULL,
    ShipAddress    nvarchar(200)  NULL,
    UpdatedAt      datetime2(0)   NOT NULL,
    CONSTRAINT PK_OrderHeader PRIMARY KEY CLUSTERED (TenantId, OrderId)
);

CREATE TABLE dbo.OrderLine
(
    TenantId       int            NOT NULL,
    OrderLineId    bigint         NOT NULL,
    OrderId        bigint         NOT NULL,
    Status         varchar(16)    NOT NULL,
    LineNum        int            NOT NULL,
    Sku            varchar(32)    NULL,
    Quantity       int            NOT NULL,
    UnitPrice      decimal(18,2)  NOT NULL,
    LineAmount     decimal(18,2)  NOT NULL,
    ProductNote    nvarchar(200)  NULL,
    UpdatedAt      datetime2(0)   NOT NULL,
    CONSTRAINT PK_OrderLine PRIMARY KEY CLUSTERED (TenantId, OrderLineId)
);

CREATE TABLE dbo.OrderSummaryDaily
(
    TenantId       int            NOT NULL,
    Status         varchar(16)    NOT NULL,
    SummaryDate    date           NOT NULL,
    OrderCount     bigint         NOT NULL,
    TotalAmount    decimal(18,2)  NOT NULL,
    Currency       char(3)        NOT NULL,
    SourceWatermark datetime2(0)  NOT NULL,
    CONSTRAINT PK_OrderSummaryDaily PRIMARY KEY CLUSTERED
        (TenantId, Status, SummaryDate)
);
