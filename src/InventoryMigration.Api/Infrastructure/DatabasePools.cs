using InventoryMigration.Api.Options;
using InventoryMigration.Api.Services;
using Microsoft.Extensions.Options;

namespace InventoryMigration.Api.Infrastructure;

public sealed class DatabasePools : IAsyncDisposable
{
    public DatabasePools(
        IOptions<MigrationOptions> options,
        FaultInjector faults)
    {
        var value = options.Value;
        Old = new DatabasePool(
            value.OldConnectionString,
            value.ConnectionLimitPerInstance,
            value.ConnectionAcquireTimeoutMs,
            value.DbCommandTimeoutSeconds,
            isOldDatabase: true,
            faults);
        New = new DatabasePool(
            value.NewConnectionString,
            value.ConnectionLimitPerInstance,
            value.ConnectionAcquireTimeoutMs,
            value.DbCommandTimeoutSeconds,
            isOldDatabase: false,
            faultInjector: null);
    }

    public DatabasePool Old { get; }

    public DatabasePool New { get; }

    public async ValueTask DisposeAsync()
    {
        await Old.DisposeAsync();
        await New.DisposeAsync();
    }
}
