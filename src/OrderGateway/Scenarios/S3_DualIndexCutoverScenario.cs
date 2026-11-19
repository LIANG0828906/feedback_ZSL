namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 3: while a legacy heartbeat is fresh the old index must remain
/// usable and retirement must be refused; after heartbeats expire the old
/// index is dropped, and a query already running during the drop completes.
/// </summary>
public sealed class DualIndexCutoverScenario : IScenario
{
    public string Name => "s3_dual_index_cutover";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var lifecycle = context.Lifecycle;
        var repository = context.Repository;
        var options = context.Options;

        var initialState = await lifecycle.GetStateAsync(cancellationToken);
        var bothIndexesPresent = initialState.OldIndexPresent && initialState.NewIndexPresent;
        var legacyActiveBefore = await lifecycle.IsLegacyStillActiveAsync(cancellationToken);

        bool retirementBlocked = false;
        try
        {
            await lifecycle.RetireOldIndexAsync(cancellationToken);
        }
        catch (GatewayException ex) when (ex.ErrorCode == "E_RETIRE_BLOCKED")
        {
            retirementBlocked = true;
        }

        var legacyQueryWhileCoexist = await repository.ReadPageAsync(
            KeyOrder.Legacy,
            new PageQuery { TenantId = 2, Status = 1, PageSize = 40 },
            cancellationToken);
        var legacyStillServed = legacyQueryWhileCoexist.UsedIndex == options.OldIndexName
            && legacyQueryWhileCoexist.Rows.Count > 0;

        await lifecycle.ExpireHeartbeatsAsync(cancellationToken);
        var legacyActiveAfterExpiry = await lifecycle.IsLegacyStillActiveAsync(cancellationToken);

        // Start a slow in-flight read first; the DDL then runs concurrently and
        // must wait on the read's schema-stability lock instead of breaking it.
        var inFlightCts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        var inFlight = Task.Run(() => RunInFlightReadDuringRetirementAsync(context, inFlightCts.Token));
        await Task.Delay(TimeSpan.FromMilliseconds(300), cancellationToken);

        await lifecycle.RetireOldIndexAsync(cancellationToken);
        var postState = await lifecycle.GetStateAsync(cancellationToken);

        var inFlightResult = await inFlight;

        var passed = bothIndexesPresent
                     && legacyActiveBefore
                     && retirementBlocked
                     && legacyStillServed
                     && !legacyActiveAfterExpiry
                     && !postState.OldIndexPresent
                     && postState.NewIndexPresent
                     && inFlightResult.CompletedWithoutError
                     && inFlightResult.RowsReturned > 0;

        return new ScenarioResult(Name, passed,
            passed ? "Old index retained while legacy alive, retired after expiry; in-flight read completed." : "Cutover sequence failed.",
            new Dictionary<string, object>
            {
                ["bothIndexesPresentInitially"] = bothIndexesPresent,
                ["retirementBlockedWhileHeartbeatFresh"] = retirementBlocked,
                ["legacyServedDuringCoexistence"] = legacyStillServed,
                ["legacyActiveAfterExpiry"] = legacyActiveAfterExpiry,
                ["oldIndexPresentAfterRetire"] = postState.OldIndexPresent,
                ["newIndexPresentAfterRetire"] = postState.NewIndexPresent,
                ["inFlightReadRows"] = inFlightResult.RowsReturned,
                ["inFlightReadClean"] = inFlightResult.CompletedWithoutError,
            });
    }

    private static async Task<(bool CompletedWithoutError, int RowsReturned)> RunInFlightReadDuringRetirementAsync(
        ScenarioContext context,
        CancellationToken cancellationToken)
    {
        try
        {
            var rows = await context.Repository.ReadPageWhileHoldingSnapshotAsync(
                KeyOrder.Legacy,
                new PageQuery { TenantId = 9, Status = 3, PageSize = 125 },
                TimeSpan.FromSeconds(2),
                cancellationToken);
            return (true, rows);
        }
        catch (GatewayException ex) when (ex.ErrorCode == GatewayError.PlanMismatch)
        {
            return (false, 0);
        }
    }
}
