USE [$(PrimaryDatabase)];
GO
SET NOCOUNT ON;
SET XACT_ABORT ON;

BEGIN TRANSACTION;

DELETE FROM dbo.OrderLines
WHERE OrderNo LIKE N'$(LoadOrderPrefix)%';

DELETE FROM dbo.Orders
WHERE OrderNo LIKE N'$(LoadOrderPrefix)%';

COMMIT TRANSACTION;
GO

