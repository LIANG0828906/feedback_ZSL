namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 6: drop the new index and restore the old one, then prove both
/// key orders still hit an index (no table scan) and that pre/post rollback
/// snapshots match row-by-row under identical filters.
/// </summary>
public sealed class RollbackScenario : IScenario
{
    public string Name => "s6_rollback_validation";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var repository = context.Repository;
        var lifecycle = context.Lifecycle;
        var options = context.Options;

        var filters = new (int Tenant, byte Status)[] { (2, 1), (8, 4), (11, 0) };

        var before = new Dictionary<string, List<OrderRow>>();
        foreach (var (tenant, status) in filters)
        {
            var rows = await PaginationWalker.WalkAllRowsAsync(
                repository, KeyOrder.New, tenant, status, 63, cancellationToken: cancellationToken);
            before[$"{tenant}:{status}"] = rows;
        }

        await lifecycle.RollbackToLegacyIndexAsync(cancellationToken);
        var postRollbackState = await lifecycle.GetStateAsync(cancellationToken);

        var postChecks = new List<object>();
        bool plansOk = true;
        bool snapshotsEqual = true;

        foreach (var (tenant, status) in filters)
        {
            var legacyPage = await repository.ReadPageAsync(
                KeyOrder.Legacy,
                new PageQuery { TenantId = tenant, Status = status, PageSize = 63 },
                cancellationToken);

            var newOrderPage = await repository.ReadPageWithExpectedIndexAsync(
                KeyOrder.New,
                new PageQuery { TenantId = tenant, Status = status, PageSize = 63 },
                options.OldIndexName,
                cancellationToken);

            bool legacyHitsOld = legacyPage.UsedIndex == options.OldIndexName
                                 && !legacyPage.AccessMethod.Contains("TABLE", StringComparison.OrdinalIgnoreCase);
            bool newOrderUsesIndex = !newOrderPage.AccessMethod.Contains("TABLE", StringComparison.OrdinalIgnoreCase)
                                     && newOrderPage.UsedIndex != "(none)";
            plansOk &= legacyHitsOld && newOrderUsesIndex;

            var afterRows = await PaginationWalker.WalkAllRowsAsync(
                repository, KeyOrder.Legacy, tenant, status, 63,
                expectedIndexOverride: options.OldIndexName,
                cancellationToken: cancellationToken);
            var key = $"{tenant}:{status}";
            bool identical = before[key]
                .OrderBy(r => r.OrderId)
                .Select(r => (r.OrderId, r.TenantId, r.Status, r.Amount, r.UpdatedAt))
                .SequenceEqual(afterRows
                    .OrderBy(r => r.OrderId)
                    .Select(r => (r.OrderId, r.TenantId, r.Status, r.Amount, r.UpdatedAt)));
            snapshotsEqual &= identical;

            postChecks.Add(new
            {
                tenantId = tenant,
                status,
                legacyIndex = legacyPage.UsedIndex,
                legacyAccess = legacyPage.AccessMethod,
                newOrderIndex = newOrderPage.UsedIndex,
                newOrderAccess = newOrderPage.AccessMethod,
                rowCount = afterRows.Count,
                snapshotIdentical = identical,
            });
        }

        bool topologyOk = !postRollbackState.NewIndexPresent && postRollbackState.OldIndexPresent;

        /* Restore the dual-index topology so the suite remains repeatable. */
        await lifecycle.RecreateNewIndexAsync(cancellationToken);
        var restoredState = await lifecycle.GetStateAsync(cancellationToken);

        var passed = plansOk && snapshotsEqual && topologyOk
                     && restoredState.OldIndexPresent && restoredState.NewIndexPresent;

        return new ScenarioResult(Name, passed,
            passed ? "Rollback topology verified; index access retained; snapshots identical row-by-row." : "Rollback verification failed.",
            new Dictionary<string, object>
            {
                ["postRollbackTopology"] = new
                {
                    oldIndexPresent = postRollbackState.OldIndexPresent,
                    newIndexPresent = postRollbackState.NewIndexPresent,
                },
                ["checks"] = postChecks,
                ["dualIndexRestoredAfterScenario"] = restoredState.OldIndexPresent && restoredState.NewIndexPresent,
            });
    }
}
