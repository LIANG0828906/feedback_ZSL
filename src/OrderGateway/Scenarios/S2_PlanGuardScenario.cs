namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 2 + plan evidence for requirement 8.
/// Happy path records the expected access path; a forced wrong-index probe must
/// be rejected with E_PLAN_MISMATCH, and actual showplans are saved as
/// repeatable evidence artifacts.
/// </summary>
public sealed class PlanGuardScenario : IScenario
{
    public string Name => "s2_plan_guard";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var repository = context.Repository;
        var options = context.Options;
        var query = new PageQuery { TenantId = 4, Status = 2, PageSize = 50 };

        var legacyPage = await repository.ReadPageAsync(KeyOrder.Legacy, query, cancellationToken);
        var newPage = await repository.ReadPageAsync(KeyOrder.New, query, cancellationToken);

        var happyPathOk =
            legacyPage.UsedIndex == options.OldIndexName &&
            legacyPage.AccessMethod.Contains("Seek", StringComparison.OrdinalIgnoreCase) &&
            newPage.UsedIndex == options.NewIndexName &&
            newPage.AccessMethod.Contains("Seek", StringComparison.OrdinalIgnoreCase);

        bool negativeRejected = false;
        GatewayException? captured = null;
        try
        {
            await repository.ReadPageWithForcedIndexAsync(
                KeyOrder.New, query, options.OldIndexName, cancellationToken);
        }
        catch (GatewayException ex) when (ex.ErrorCode == GatewayError.PlanMismatch)
        {
            negativeRejected = true;
            captured = ex;
        }

        var evidence = new Dictionary<string, object>
        {
            ["happyPath"] = new
            {
                legacyIndex = legacyPage.UsedIndex,
                legacyAccess = legacyPage.AccessMethod,
                newIndex = newPage.UsedIndex,
                newAccess = newPage.AccessMethod,
            },
            ["negativeProbe"] = new
            {
                rejectedWithPlanMismatch = negativeRejected,
                errorCode = captured?.ErrorCode,
                context = captured?.Context,
            },
            ["planEvidenceFiles"] = new[]
            {
                $"actual_plan_{KeyOrder.Legacy.AsLabel()}.sqlplan",
                $"actual_plan_{KeyOrder.New.AsLabel()}.sqlplan",
            },
        };

        var passed = happyPathOk && negativeRejected;
        return new ScenarioResult(
            Name,
            passed,
            passed ? "Expected seeks observed; wrong-index probe rejected with E_PLAN_MISMATCH." : "Plan guard did not behave as required.",
            evidence);
    }
}
