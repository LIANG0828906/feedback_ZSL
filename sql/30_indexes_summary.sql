SET NOCOUNT ON;
SET XACT_ABORT ON;

CREATE INDEX IX_OrderHeader_Tenant_Status_OrderDate
    ON dbo.OrderHeader (TenantId, Status, OrderDate, OrderId)
    INCLUDE (OrderNo, CustomerName, Amount, Currency, UpdatedAt);

CREATE INDEX IX_OrderHeader_Tenant_Status_OrderNo
    ON dbo.OrderHeader (TenantId, Status, OrderNo)
    INCLUDE (OrderDate, OrderId, CustomerName, Amount, Currency, UpdatedAt);

CREATE INDEX IX_OrderLine_Tenant_Status_OrderId
    ON dbo.OrderLine (TenantId, Status, OrderId, LineNum)
    INCLUDE (Sku, Quantity, UnitPrice, LineAmount, UpdatedAt);

CREATE INDEX IX_OrderLine_Tenant_Status_Sku
    ON dbo.OrderLine (TenantId, Status, Sku)
    INCLUDE (OrderId, LineNum, Quantity, UnitPrice, LineAmount, UpdatedAt);

CREATE INDEX IX_OrderHeader_CustomerName
    ON dbo.OrderHeader (CustomerName);

CREATE INDEX IX_OrderLine_ProductNote
    ON dbo.OrderLine (ProductNote);

UPDATE STATISTICS dbo.OrderHeader WITH FULLSCAN;
UPDATE STATISTICS dbo.OrderLine WITH FULLSCAN;
UPDATE STATISTICS dbo.OrderSummaryDaily WITH FULLSCAN;
