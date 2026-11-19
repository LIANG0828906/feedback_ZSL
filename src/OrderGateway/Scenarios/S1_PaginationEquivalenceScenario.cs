namespace OrderGateway.Scenarios;

/// <summary>
/// Requirement 1: both key orders must return exactly the same order-id
/// multiset for every filter and every page size - order may differ only.
/// </summary>
public sealed class PaginationEquivalenceScenario : IScenario
{
    public string Name => "s1_pagination_equivalence";

    public async Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken)
    {
        var repository = context.Repository;
        var filters = new (int Tenant, byte Status)[]
        {
            (0, 0), (3, 2), (7, 4), (11, 1), (5, 3),
        };
        var pageSizes = new[] { 1, 7, 25, 100, 500 };
        var checks = new List<object>();
        bool passed = true;

        foreach (var (tenant, status) in filters)
        {
            foreach (var pageSize in pageSizes)
            {
                var legacyIds = await PaginationWalker.WalkAllOrderIdsAsync(
                    repository, KeyOrder.Legacy, tenant, status, pageSize, cancellationToken);
                var newIds = await PaginationWalker.WalkAllOrderIdsAsync(
                    repository, KeyOrder.New, tenant, status, pageSize, cancellationToken);

                bool sameSet = legacyIds.Count == newIds.Count
                    && legacyIds.OrderBy(x => x).SequenceEqual(newIds.OrderBy(x => x));
                bool legacySorted = legacyIds.SequenceEqual(legacyIds.OrderBy(x => x));

                if (!sameSet || !legacySorted) passed = false;

                checks.Add(new
                {
                    tenantId = tenant,
                    status,
                    pageSize,
                    rowCount = legacyIds.Count,
                    sameMultiset = sameSet,
                    legacyOrderDistinctFromNew = !legacyIds.SequenceEqual(newIds),
                });
            }
        }

        return new ScenarioResult(
            Name,
            passed,
            passed ? "Both key orders return identical multisets on every filter/page size." : "Multiset divergence detected.",
            new Dictionary<string, object> { ["checks"] = checks });
    }
}
