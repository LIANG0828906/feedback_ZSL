using System.Data;
using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>Builds keyset-pagination SQL for both logical key orders.</summary>
public sealed class QueryBuilder
{
    private readonly GatewayOptions _options;

    public QueryBuilder(GatewayOptions options) => _options = options;

    public (string Sql, List<SqlParameter> Parameters) Build(
        KeyOrder keyOrder,
        PageQuery query,
        string? forcedIndexName)
    {
        var targetIndex = ResolveTargetIndex(keyOrder, forcedIndexName);
        var parameters = new List<SqlParameter>
        {
            new("@TenantId", SqlDbType.Int) { Value = query.TenantId },
            new("@Status", SqlDbType.TinyInt) { Value = (byte)query.Status },
            new("@PageSize", SqlDbType.Int) { Value = (int)(query.PageSize + 1) },
        };

        if (keyOrder == KeyOrder.Legacy)
        {
            string cursorClause;
            if (query.Cursor is LegacyCursor legacy)
            {
                cursorClause = "AND OrderId > @LastOrderId";
                parameters.Add(new SqlParameter("@LastOrderId", SqlDbType.BigInt) { Value = legacy.LastOrderId });
            }
            else
            {
                cursorClause = string.Empty;
            }

            var sql = $"""
                SELECT TOP (@PageSize)
                    OrderId, TenantId, Status, Amount, UpdatedAt
                FROM dbo.Orders {IndexHint(targetIndex)}
                WHERE Status = @Status
                  AND TenantId = @TenantId
                  {cursorClause}
                ORDER BY Status ASC, OrderId ASC
                OPTION (RECOMPILE);
                """;
            return (sql, parameters);
        }

        string newCursorClause;
        if (query.Cursor is NewCursor fresh)
        {
            newCursorClause = """
                AND (UpdatedAt > @LastUpdatedAt
                     OR (UpdatedAt = @LastUpdatedAt AND OrderId > @LastOrderId))
                """;
            parameters.Add(new SqlParameter("@LastUpdatedAt", SqlDbType.DateTime2)
            {
                Value = fresh.UpdatedAt
            });
            parameters.Add(new SqlParameter("@LastOrderId", SqlDbType.BigInt) { Value = fresh.LastOrderId });
        }
        else
        {
            newCursorClause = string.Empty;
        }

        var newSql = $"""
            SELECT TOP (@PageSize)
                OrderId, TenantId, Status, Amount, UpdatedAt
            FROM dbo.Orders {IndexHint(targetIndex)}
            WHERE TenantId = @TenantId
              AND Status = @Status
              {newCursorClause}
            ORDER BY TenantId ASC, Status ASC, UpdatedAt ASC, OrderId ASC
            OPTION (RECOMPILE);
            """;
        return (newSql, parameters);
    }

    private string ResolveTargetIndex(KeyOrder keyOrder, string? forcedIndexName)
    {
        if (forcedIndexName is not null) return forcedIndexName;
        return keyOrder == KeyOrder.Legacy ? _options.OldIndexName : _options.NewIndexName;
    }

    private static string IndexHint(string indexName) =>
        string.IsNullOrWhiteSpace(indexName)
            ? "WITH (FORCESEEK)"
            : $"WITH (INDEX({indexName}), FORCESEEK)";
}
