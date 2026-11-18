using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;

namespace SnapshotGateway.Services;

public sealed record TransitionReconcileResult(
    long PrimaryVersion,
    IReadOnlyList<ReplicaVersionInfo> Versions,
    double CatchupDurationMs,
    bool Aligned,
    string? Error);

public sealed class SnapshotCoordinator
{
    private readonly ReplicaManager _replicas;
    private readonly GatewayMetrics _metrics;

    public SnapshotCoordinator(ReplicaManager replicas, GatewayMetrics metrics)
    {
        _replicas = replicas;
        _metrics = metrics;
    }

    public async Task<SnapshotDecision> EnsureConsistentAsync(CancellationToken cancellationToken)
    {
        var initialPrimaryVersion = await _replicas.ReadPrimaryDataVersionAsync(cancellationToken);
        var initialVersions = await _replicas.ReadAllVersionsAsync(cancellationToken);
        if (IsAligned(initialVersions, initialPrimaryVersion))
        {
            return new SnapshotDecision(
                Consistent: true,
                initialPrimaryVersion,
                initialVersions,
                initialVersions,
                CatchupAttempted: false,
                CatchupCompleted: false,
                CatchupDurationMs: 0);
        }

        var versionsBefore = initialVersions;
        var after = initialVersions;
        var primaryVersion = initialPrimaryVersion;
        var totalCatchupMs = 0d;
        var catchupAttempted = false;
        var catchupCompleted = false;

        for (var attempt = 0; attempt < 2; attempt++)
        {
            primaryVersion = await _replicas.ReadPrimaryDataVersionAsync(cancellationToken);
            var result = await _replicas.CatchUpAllAsync(primaryVersion, cancellationToken);
            catchupAttempted |= result.Started;
            catchupCompleted = result.Completed;
            totalCatchupMs += result.TotalDurationMs;
            after = result.VersionsAfter;
            if (IsAligned(after, primaryVersion))
            {
                break;
            }
        }

        return new SnapshotDecision(
            Consistent: false,
            PrimaryVersion: primaryVersion,
            VersionsBefore: versionsBefore,
            VersionsAfter: after,
            CatchupAttempted: catchupAttempted,
            CatchupCompleted: catchupCompleted,
            CatchupDurationMs: totalCatchupMs);
    }

    public async Task<TransitionReconcileResult> ReconcileAfterTransitionAsync(
        CancellationToken cancellationToken)
    {
        try
        {
            var totalCatchupMs = 0d;
            string? error = null;
            var primaryVersion = await _replicas.ReadPrimaryDataVersionAsync(cancellationToken);
            var versions = await _replicas.ReadAllVersionsAsync(cancellationToken);

            for (var attempt = 0; attempt < 2 && !IsAligned(versions, primaryVersion); attempt++)
            {
                var result = await _replicas.CatchUpAllAsync(primaryVersion, cancellationToken);
                totalCatchupMs += result.TotalDurationMs;
                error = result.Error;
                versions = result.VersionsAfter;
                primaryVersion = await _replicas.ReadPrimaryDataVersionAsync(cancellationToken);
            }

            var aligned = IsAligned(versions, primaryVersion);
            return new TransitionReconcileResult(
                primaryVersion,
                versions,
                totalCatchupMs,
                aligned,
                aligned ? error : error ?? "Replica snapshots remain misaligned.");
        }
        catch (OperationCanceledException)
        {
            throw;
        }
        catch (Exception ex)
        {
            return new TransitionReconcileResult(
                -1,
                Array.Empty<ReplicaVersionInfo>(),
                0,
                false,
                ex.Message);
        }
    }

    private static bool IsAligned(IReadOnlyList<ReplicaVersionInfo> versions, long targetVersion) =>
        versions.Count > 0
        && versions.All(version => version.SnapshotVersion == targetVersion);
}
