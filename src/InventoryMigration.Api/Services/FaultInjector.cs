using InventoryMigration.Api.Domain;

namespace InventoryMigration.Api.Services;

public sealed class FaultInjector
{
    private readonly object _sync = new();
    private volatile bool _oldDbUnavailable;
    private int _migrationBatchDelayMs;
    private HashSet<long> _validationCorruptProductIds = [];

    public bool OldDbUnavailable => _oldDbUnavailable;

    public int MigrationBatchDelayMs => Volatile.Read(ref _migrationBatchDelayMs);

    public void Set(FaultRequest request)
    {
        lock (_sync)
        {
            if (request.OldDbUnavailable.HasValue)
            {
                _oldDbUnavailable = request.OldDbUnavailable.Value;
            }

            if (request.MigrationBatchDelayMs.HasValue)
            {
                Volatile.Write(
                    ref _migrationBatchDelayMs,
                    Math.Clamp(request.MigrationBatchDelayMs.Value, 0, 60_000));
            }

            if (request.ValidationCorruptProductIds is not null)
            {
                _validationCorruptProductIds = request.ValidationCorruptProductIds
                    .Where(id => id > 0)
                    .ToHashSet();
            }
        }
    }

    public IReadOnlySet<long> GetValidationCorruptionProducts()
    {
        lock (_sync)
        {
            return _validationCorruptProductIds.ToHashSet();
        }
    }

    public void Clear()
    {
        lock (_sync)
        {
            _oldDbUnavailable = false;
            Volatile.Write(ref _migrationBatchDelayMs, 0);
            _validationCorruptProductIds = [];
        }
    }
}
