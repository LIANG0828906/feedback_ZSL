SET NOCOUNT ON;

USE [$(PrimaryDatabase)];
GO

SELECT
    N'primary' AS role_name,
    DB_NAME() AS database_name,
    DATABASEPROPERTYEX(DB_NAME(), 'Updateability') AS updateability,
    (SELECT COUNT_BIG(*) FROM dbo.Orders) AS order_rows,
    (SELECT COUNT_BIG(*) FROM dbo.OrderLines) AS order_line_rows,
    (SELECT AppliedWatermark FROM dbo.ConsistencyWatermark
     WHERE Name = N'orders') AS applied_watermark;

DECLARE @replica_query nvarchar(max) = N'
    SELECT N''replica'' AS role_name,
        N''' + REPLACE(N'$(ReplicaDatabase)', N'''', N'''''') + N''' AS database_name,
        DATABASEPROPERTYEX(N''' + REPLACE(N'$(ReplicaDatabase)', N'''', N'''''') + N''', ''Updateability'') AS updateability,
        (SELECT COUNT_BIG(*) FROM [' + QUOTENAME(N'$(ReplicaDatabase)') + N'].dbo.Orders) AS order_rows,
        (SELECT COUNT_BIG(*) FROM [' + QUOTENAME(N'$(ReplicaDatabase)') + N'].dbo.OrderLines) AS order_line_rows,
        (SELECT AppliedWatermark FROM [' + QUOTENAME(N'$(ReplicaDatabase)') + N'].dbo.ConsistencyWatermark
         WHERE Name = N''orders'') AS applied_watermark;';
EXEC sys.sp_executesql @replica_query;
GO

