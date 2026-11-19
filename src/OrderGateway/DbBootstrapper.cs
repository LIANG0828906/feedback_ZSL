using Microsoft.Data.SqlClient;

namespace OrderGateway;

/// <summary>
/// Executes the deterministic init SQL. The repository intentionally avoids a
/// sqlcmd dependency, so the only sqlcmd variable used ($(DatabaseName)) is
/// substituted here after identifier escaping.
/// </summary>
public sealed class DbBootstrapper
{
    private readonly GatewayOptions _options;

    public DbBootstrapper(GatewayOptions options) => _options = options;

    public async Task InitializeAsync(string initSqlPath, CancellationToken cancellationToken = default)
    {
        if (!File.Exists(initSqlPath))
            throw new FileNotFoundException("Deterministic init script not found", initSqlPath);

        var sql = await File.ReadAllTextAsync(initSqlPath, cancellationToken).ConfigureAwait(false);
        var database = SanitizeIdentifier(_options.DatabaseName);
        var masterBuilder = new SqlConnectionStringBuilder(_options.ConnectionString)
        {
            InitialCatalog = "master",
            Pooling = false,
        };

        await using var connection = new SqlConnection(masterBuilder.ConnectionString);
        await connection.OpenAsync(cancellationToken).ConfigureAwait(false);

        var batches = SplitBatches(sql).ToList();
        foreach (var batch in batches)
        {
            if (string.IsNullOrWhiteSpace(batch)) continue;

            var applied = batch.Replace("$(DatabaseName)", database);
            var isCatalogSwitch = applied.TrimStart()
                .StartsWith("USE ", StringComparison.OrdinalIgnoreCase)
                && applied.Contains(database, StringComparison.Ordinal);

            await using var command = connection.CreateCommand();
            command.CommandText = applied;
            command.CommandTimeout = 120;

            if (isCatalogSwitch)
            {
                await connection.ChangeDatabaseAsync(database, cancellationToken).ConfigureAwait(false);
                continue;
            }

            await command.ExecuteNonQueryAsync(cancellationToken).ConfigureAwait(false);
        }
    }

    public async Task<long> CountOrdersAsync(CancellationToken cancellationToken = default)
    {
        await using var connection = new SqlConnection(_options.ConnectionString);
        await connection.OpenAsync(cancellationToken).ConfigureAwait(false);
        await using var command = connection.CreateCommand();
        command.CommandText = "SELECT COUNT_BIG(*) FROM dbo.Orders;";
        return (long)(await command.ExecuteScalarAsync(cancellationToken).ConfigureAwait(false))!;
    }

    private static string SanitizeIdentifier(string name)
    {
        if (string.IsNullOrWhiteSpace(name) || name.Any(c => c is ']' or ';' or '-' or ' '))
            throw new InvalidOperationException($"Unsafe database identifier: {name}");
        return name;
    }

    private static IEnumerable<string> SplitBatches(string sql)
    {
        var lines = sql.Replace("\r\n", "\n").Split('\n');
        var batch = new System.Text.StringBuilder();
        bool inBlockComment = false;

        foreach (var rawLine in lines)
        {
            var line = rawLine;

            if (!inBlockComment && line.Trim().Equals("GO", StringComparison.OrdinalIgnoreCase))
            {
                if (batch.Length > 0)
                {
                    yield return batch.ToString();
                    batch.Clear();
                }
                continue;
            }

            if (inBlockComment)
            {
                var close = line.IndexOf("*/", StringComparison.Ordinal);
                if (close < 0) continue;
                inBlockComment = false;
                line = line[(close + 2)..];
            }

            var code = StripCommentsFromLine(line, ref inBlockComment);
            if (!string.IsNullOrWhiteSpace(code)) batch.AppendLine(code);
        }

        if (batch.Length > 0) yield return batch.ToString();
    }

    private static string StripCommentsFromLine(string line, ref bool inBlockComment)
    {
        var result = new System.Text.StringBuilder(line.Length);
        int index = 0;
        while (index < line.Length)
        {
            if (!inBlockComment && index + 1 < line.Length && line[index] == '/' && line[index + 1] == '*')
            {
                inBlockComment = true;
                index += 2;
                continue;
            }

            if (inBlockComment)
            {
                if (index + 1 < line.Length && line[index] == '*' && line[index + 1] == '/')
                {
                    inBlockComment = false;
                    index += 2;
                }
                else
                {
                    index++;
                }
                continue;
            }

            if (index + 1 < line.Length && line[index] == '-' && line[index + 1] == '-')
                break;

            result.Append(line[index]);
            index++;
        }
        return result.ToString();
    }
}
