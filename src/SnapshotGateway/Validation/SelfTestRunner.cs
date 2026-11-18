using System.Collections.Concurrent;
using System.Diagnostics;
using SnapshotGateway.Configuration;
using SnapshotGateway.Domain;
using SnapshotGateway.Infrastructure;
using SnapshotGateway.Services;

namespace SnapshotGateway.Validation;

public sealed record SelfTestCheck(string Name, bool Passed, string Detail);

public sealed class SelfTestRunner
{
    private readonly GatewayOptions _options;
    private readonly OrderQueryService _queries;
    private readonly IndexStateService _indexes;
    private readonly OrderWriter _writer;
    private readonly SnapshotCoordinator _coordinator;
    private readonly ReplicaManager _replicas;
    private readonly GatewayMetrics _metrics;
    private readonly ReportService _report;
    private readonly SqliteConnectionFactory _factory;

    public SelfTestRunner(
        GatewayOptions options,
        OrderQueryService queries,
        IndexStateService indexes,
        OrderWriter writer,
        SnapshotCoordinator coordinator,
        ReplicaManager replicas,
        GatewayMetrics metrics,
        ReportService report,
        SqliteConnectionFactory factory)
    {
        _options = options;
        _queries = queries;
        _indexes = indexes;
        _writer = writer;
        _coordinator = coordinator;
        _replicas = replicas;
        _metrics = metrics;
        _report = report;
        _factory = factory;
    }

    public async Task<int> RunAsync(CancellationToken cancellationToken = default)
    {
        var checks = new List<SelfTestCheck>();
        try
        {
            var state = await _indexes.GetStateAsync(cancellationToken);
            string sqliteVersion;
            await using (var lease = await _factory.OpenAsync(
                             _options.PrimaryDatabasePath, readOnly: true, cancellationToken))
            {
                await using var versionCommand = lease.Connection.CreateCommand();
                versionCommand.CommandText = "SELECT sqlite_version();";
                sqliteVersion = Convert.ToString(
                    await versionCommand.ExecuteScalarAsync(cancellationToken)) ?? "unknown";
            }

            Add(checks, "topology.sqlite-3.42",
                sqliteVersion == "3.42.0",
                $"sqlite_version()={sqliteVersion}");
            Add(checks, "topology.initialized",
                state.OldIndexName == OrderQueryService.OldIndexName
                && state.NewIndexName == OrderQueryService.NewIndexName,
                $"old={state.OldIndexName}, new={state.NewIndexName}, snapshot={state.DataSnapshotVersion}");

            const string tenant = "T001";
            const string status = "NEW";
            const int pageSize = 37;
            var oldPages = await CollectPagesAsync(tenant, status, KeyOrders.Old, pageSize, null,
                cancellationToken);
            var newPages = await CollectPagesAsync(tenant, status, KeyOrders.New, pageSize, null,
                cancellationToken);
            var oldRows = oldPages.SelectMany(page => page.Page!.Items).ToArray();
            var newRows = newPages.SelectMany(page => page.Page!.Items).ToArray();
            Add(checks, "orders.seed-count",
                oldPages.All(page => page.Page!.TotalCount == 250),
                $"filter={tenant}/{status}, total={oldPages[0].Page!.TotalCount}");
            Add(checks, "dual-key-order.same-row-set",
                SameRows(oldRows, newRows),
                $"oldRows={oldRows.Length}, newRows={newRows.Length}, " +
                $"oldDistinct={oldRows.Select(row => row.OrderId).Distinct().Count()}, " +
                $"newDistinct={newRows.Select(row => row.OrderId).Distinct().Count()}");
            Add(checks, "dual-key-order.order-differs",
                !oldRows.Select(row => row.OrderId).SequenceEqual(newRows.Select(row => row.OrderId)),
                "The two key orders intentionally produce different row sequences.");
            Add(checks, "dual-key-order.all-filtered",
                oldRows.Concat(newRows).All(row => row.TenantId == tenant && row.Status == status),
                "Every returned row satisfies tenant/status filters.");
            Add(checks, "plans.expected-indexes",
                oldPages.All(page => page.Plan!.Matched
                                     && page.Plan.ExpectedIndex == OrderQueryService.OldIndexName)
                && newPages.All(page => page.Plan!.Matched
                                       && page.Plan.ExpectedIndex == OrderQueryService.NewIndexName),
                "Every paginated request used its expected index with SEARCH access.");
            Add(checks, "plans.no-table-scan",
                oldPages.Concat(newPages).All(page => page.Plan!.NoTableScan),
                "No plan line performs a full table scan of orders.");

            var replicaResults = new List<QueryResult>();
            for (var replicaId = 1; replicaId <= _replicas.Replicas.Count; replicaId++)
            {
                var result = await RequireQueryAsync(
                    tenant, status, KeyOrders.Old, 1, pageSize, replicaId, cancellationToken);
                replicaResults.Add(result);
            }

            Add(checks, "replicas.same-snapshot-and-page",
                replicaResults.Select(result => result.SnapshotVersion).Distinct().Count() == 1
                && replicaResults.Select(result =>
                        string.Join(",", result.Page!.Items.Select(row => row.OrderId)))
                    .Distinct().Count() == 1,
                string.Join("; ", replicaResults.Select(result =>
                    $"replica-{result.ReplicaId}:v{result.SnapshotVersion}")));

            var mismatch = await _queries.QueryAsync(new QueryFilter
            {
                TenantId = tenant,
                Status = status,
                KeyOrder = KeyOrders.Old,
                Page = 1,
                PageSize = 25,
                ExpectedIndexOverride = OrderQueryService.NewIndexName
            }, cancellationToken);
            Add(checks, "plan-mismatch.rejected",
                !mismatch.Success
                && mismatch.ErrorCode == ErrorCodes.PlanMismatch
                && mismatch.Plan is { Lines.Count: > 0 },
                $"error={mismatch.ErrorCode}, planLines={mismatch.Plan?.Lines.Count ?? 0}");

            var update = await _writer.UpdateBatchAsync(20, cancellationToken);
            var lag = await _queries.QueryAsync(new QueryFilter
            {
                TenantId = tenant,
                Status = status,
                KeyOrder = KeyOrders.New,
                Page = 1,
                PageSize = 25
            }, cancellationToken);
            Add(checks, "snapshot-lag.reported",
                !lag.Success && lag.ErrorCode == ErrorCodes.SnapshotLag,
                $"writeVersion={update.SnapshotVersion}, error={lag.ErrorCode}, " +
                $"replicas={string.Join(",", lag.ReplicaVersions.Select(replica => replica.SnapshotVersion))}");
            var retry = await RequireQueryAsync(
                tenant, status, KeyOrders.New, 1, 25, null, cancellationToken);
            Add(checks, "snapshot-lag.caught-up",
                retry.SnapshotVersion == update.SnapshotVersion,
                $"snapshot={retry.SnapshotVersion}, expected={update.SnapshotVersion}");
            var failures = _metrics.Snapshot().Failures;
            Add(checks, "failure-log.contains-key-order-and-filter",
                failures.Any(record =>
                    record.ErrorCode == ErrorCodes.PlanMismatch
                    && record.KeyOrder == KeyOrders.Old
                    && record.TenantId == tenant
                    && record.Status == status)
                && failures.Any(record =>
                    record.ErrorCode == ErrorCodes.SnapshotLag
                    && record.TenantId == tenant
                    && record.Status == status),
                $"failureRecords={failures.Count}");

            var preSwitch = await CollectPagesAsync(
                tenant, status, KeyOrders.Old, 50, null, cancellationToken);
            var preSwitchVersion = preSwitch[0].SnapshotVersion;
            var switched = await _indexes.SwitchToNewAsync(cancellationToken);
            Add(checks, "switch.atomic-audit",
                switched.Success
                && switched.State is { ActiveGeneration: "NEW", OldIndexState: "ACTIVE_LEGACY", NewIndexState: "ACTIVE" }
                && switched.AuditId > 0,
                $"audit={switched.AuditId}, state={switched.State?.ActiveGeneration}, " +
                $"old={switched.State?.OldIndexState}, new={switched.State?.NewIndexState}");

            var afterSwitchOld = await CollectPagesAsync(
                tenant, status, KeyOrders.Old, 50, null, cancellationToken);
            var afterSwitchNew = await CollectPagesAsync(
                tenant, status, KeyOrders.New, 50, null, cancellationToken);
            Add(checks, "switch.same-snapshot",
                afterSwitchOld.All(page => page.SnapshotVersion == preSwitchVersion)
                && afterSwitchNew.All(page => page.SnapshotVersion == preSwitchVersion),
                $"pre={preSwitchVersion}, old={afterSwitchOld[0].SnapshotVersion}, " +
                $"new={afterSwitchNew[0].SnapshotVersion}");
            Add(checks, "switch.rollback-data-preserved",
                SameRows(
                    preSwitch.SelectMany(page => page.Page!.Items).ToArray(),
                    afterSwitchOld.SelectMany(page => page.Page!.Items).ToArray())
                && SameRows(
                    afterSwitchOld.SelectMany(page => page.Page!.Items).ToArray(),
                    afterSwitchNew.SelectMany(page => page.Page!.Items).ToArray()),
                "Old/new key orders return identical rows after switch.");

            var beforeRollbackOld = await CollectPagesAsync(
                tenant, status, KeyOrders.Old, 50, null, cancellationToken);
            var beforeRollbackNew = await CollectPagesAsync(
                tenant, status, KeyOrders.New, 50, null, cancellationToken);
            var rolledBack = await _indexes.RollbackAsync(cancellationToken);
            var afterRollbackOld = await CollectPagesAsync(
                tenant, status, KeyOrders.Old, 50, null, cancellationToken);
            var afterRollbackNew = await CollectPagesAsync(
                tenant, status, KeyOrders.New, 50, null, cancellationToken);
            Add(checks, "rollback.atomic-audit",
                rolledBack.Success
                && rolledBack.State is { ActiveGeneration: "OLD", OldIndexState: "ACTIVE", NewIndexState: "READY" }
                && rolledBack.AuditId > switched.AuditId,
                $"audit={rolledBack.AuditId}, state={rolledBack.State?.ActiveGeneration}, " +
                $"old={rolledBack.State?.OldIndexState}, new={rolledBack.State?.NewIndexState}");
            Add(checks, "rollback.same-snapshot-row-by-row",
                beforeRollbackOld.SelectMany(page => page.Page!.Items)
                    .SequenceEqual(afterRollbackOld.SelectMany(page => page.Page!.Items))
                && beforeRollbackNew.SelectMany(page => page.Page!.Items)
                    .SequenceEqual(afterRollbackNew.SelectMany(page => page.Page!.Items))
                && afterRollbackOld.All(page => page.SnapshotVersion == preSwitchVersion)
                && afterRollbackNew.All(page => page.SnapshotVersion == preSwitchVersion),
                $"snapshot={afterRollbackOld[0].SnapshotVersion}, rows={afterRollbackOld.Sum(page => page.Page!.Items.Count)}");

            var audit = await _indexes.GetAuditAsync(20, cancellationToken);
            Add(checks, "audit.switch-and-rollback",
                audit.Any(record => record.Operation == "SWITCH" && record.Result == "COMMITTED")
                && audit.Any(record => record.Operation == "ROLLBACK" && record.Result == "COMMITTED"),
                $"auditRows={audit.Count}, latest={audit.FirstOrDefault()?.Operation}");

            var concurrent = await RunConcurrencyProbeAsync(cancellationToken);
            var probeDurations = concurrent
                .Select(result => result.ExecutionDurationMs ?? result.DurationMs)
                .ToArray();
            var p95 = P95(probeDurations);
            _metrics.SetPerformanceProbe(new PerformanceProbe(
                "six-way-concurrent-indexed-pagination",
                6,
                concurrent.Count,
                p95,
                _options.PerfThresholdMs,
                p95 <= _options.PerfThresholdMs,
                probeDurations.OrderBy(value => value).ToArray()));
            Add(checks, "performance.p95-under-budget",
                p95 <= _options.PerfThresholdMs,
                $"6 concurrent reads, samples={concurrent.Count}, p95={p95:F3}ms, " +
                $"threshold={_options.PerfThresholdMs:F0}ms");

            var mixed = await RunMixedLoadAsync(cancellationToken);
            Add(checks, "mixed-load.pages-distinct-and-complete",
                mixed.All(result => result.Passed),
                $"successfulPages={mixed.Count}, failures={mixed.Count(result => !result.Passed)}");

            var hold = new List<SqliteLease>();
            try
            {
                for (var i = 0; i < _factory.MaxConnections; i++)
                {
                    hold.Add(await _factory.OpenAsync(
                        _options.PrimaryDatabasePath, readOnly: true, cancellationToken));
                }

                var limited = await _queries.QueryAsync(new QueryFilter
                {
                    TenantId = tenant,
                    Status = status,
                    KeyOrder = KeyOrders.Old,
                    Page = 1,
                    PageSize = 10
                }, cancellationToken);
                Add(checks, "connections.limit-rejected",
                    !limited.Success && limited.ErrorCode == ErrorCodes.ConnectionLimit,
                    $"max={_factory.MaxConnections}, error={limited.ErrorCode}");
            }
            finally
            {
                foreach (var lease in hold)
                {
                    await lease.DisposeAsync();
                }
            }

            await _coordinator.ReconcileAfterTransitionAsync(cancellationToken);
            var report = await _report.BuildAsync(new
            {
                passed = checks.All(check => check.Passed),
                checks
            }, cancellationToken);
            var reportPath = await _report.WriteAsync(report, _options.ReportPath, cancellationToken);
            var json = ReportService.Serialize(report);
            Console.Out.WriteLine(json);
            Console.Error.WriteLine($"Report written to {reportPath}");
            return checks.All(check => check.Passed) ? 0 : 1;
        }
        catch (Exception ex)
        {
            checks.Add(new SelfTestCheck("self-test.exception", false, ex.ToString()));
            var report = await _report.BuildAsync(new
            {
                passed = false,
                checks
            }, CancellationToken.None);
            await _report.WriteAsync(report, _options.ReportPath, CancellationToken.None);
            Console.Out.WriteLine(ReportService.Serialize(report));
            Console.Error.WriteLine(ex);
            return 1;
        }
    }

    private async Task<List<QueryResult>> CollectPagesAsync(
        string tenant,
        string status,
        string keyOrder,
        int pageSize,
        int? replicaId,
        CancellationToken cancellationToken)
    {
        var pages = new List<QueryResult>();
        var page = 1;
        while (true)
        {
            var result = await RequireQueryAsync(
                tenant, status, keyOrder, page, pageSize, replicaId, cancellationToken);
            pages.Add(result);
            var seen = (page - 1) * pageSize + result.Page!.Items.Count;
            if (seen >= result.Page.TotalCount || result.Page.Items.Count == 0)
            {
                break;
            }

            page++;
            if (page > 10_000)
            {
                throw new InvalidOperationException("Pagination did not terminate.");
            }
        }

        return pages;
    }

    private async Task<QueryResult> RequireQueryAsync(
        string tenant,
        string status,
        string keyOrder,
        int page,
        int pageSize,
        int? replicaId,
        CancellationToken cancellationToken)
    {
        QueryResult? last = null;
        for (var attempt = 0; attempt < 20; attempt++)
        {
            last = await _queries.QueryAsync(new QueryFilter
            {
                TenantId = tenant,
                Status = status,
                KeyOrder = keyOrder,
                Page = page,
                PageSize = pageSize,
                ReplicaId = replicaId
            }, cancellationToken);
            if (last.Success)
            {
                return last;
            }

            if (last.ErrorCode != ErrorCodes.SnapshotLag
                && last.ErrorCode != ErrorCodes.ConnectionLimit)
            {
                break;
            }

            await Task.Delay(15, cancellationToken);
        }

        throw new InvalidOperationException(
            $"Query failed after retries: {last?.ErrorCode} {last?.Message}");
    }

    private async Task<List<QueryResult>> RunConcurrencyProbeAsync(
        CancellationToken cancellationToken)
    {
        var start = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var results = new ConcurrentBag<QueryResult>();
        var tasks = Enumerable.Range(0, 6).Select(worker => Task.Run(async () =>
        {
            await start.Task;
            for (var index = 0; index < 8; index++)
            {
                var keyOrder = (worker + index) % 2 == 0 ? KeyOrders.Old : KeyOrders.New;
                var result = await RequireQueryAsync(
                    $"T{(worker % 20) + 1:D3}",
                    index % 2 == 0 ? "NEW" : "PROCESSING",
                    keyOrder,
                    (index % 4) + 1,
                    _options.DefaultPageSize,
                    null,
                    cancellationToken);
                results.Add(result);
            }
        }, cancellationToken)).ToArray();
        start.SetResult();
        await Task.WhenAll(tasks);
        return results.ToList();
    }

    private async Task<List<(bool Passed, string Detail)>> RunMixedLoadAsync(
        CancellationToken cancellationToken)
    {
        var writer = Task.Run(async () =>
        {
            for (var i = 0; i < 4; i++)
            {
                await _writer.UpdateBatchAsync(15, cancellationToken);
                await Task.Delay(20, cancellationToken);
            }
        }, cancellationToken);

        var readerTasks = Enumerable.Range(0, 6).Select(worker => Task.Run(async () =>
        {
            var validation = new List<(bool Passed, string Detail)>();
            var successes = 0;
            for (var attempt = 0; attempt < 40 && successes < 6; attempt++)
            {
                var keyOrder = (worker + attempt) % 2 == 0 ? KeyOrders.Old : KeyOrders.New;
                var result = await _queries.QueryAsync(new QueryFilter
                {
                    TenantId = $"T{(worker % 20) + 1:D3}",
                    Status = "SHIPPED",
                    KeyOrder = keyOrder,
                    Page = (attempt % 5) + 1,
                    PageSize = 25
                }, cancellationToken);
                if (!result.Success)
                {
                    if (result.ErrorCode is ErrorCodes.SnapshotLag or ErrorCodes.ConnectionLimit)
                    {
                        await Task.Delay(10, cancellationToken);
                        continue;
                    }

                    validation.Add((false, $"unexpected error={result.ErrorCode}"));
                    break;
                }

                successes++;
                var expectedItems = Math.Max(
                    0,
                    Math.Min(
                        result.Filter!.PageSize,
                        (int)(result.Page!.TotalCount - result.Page.Offset)));
                var ids = result.Page.Items.Select(row => row.OrderId).ToArray();
                var distinct = ids.Distinct().Count();
                var filtersOk = result.Page.Items.All(row =>
                    row.TenantId == result.Filter.TenantId
                    && row.Status == result.Filter.Status);
                var planOk = result.Plan is
                {
                    Matched: true,
                    NoTableScan: true
                } && result.Plan.ExpectedIndex ==
                    OrderQueryService.ExpectedIndexFor(result.KeyOrder);
                validation.Add((
                    distinct == ids.Length && ids.Length == expectedItems && filtersOk && planOk,
                    $"{result.KeyOrder}:p{result.Page.Page}:snapshot={result.SnapshotVersion}:" +
                    $"rows={ids.Length}/{expectedItems}:distinct={distinct}:plan={planOk}"));
            }

            if (successes == 0)
            {
                validation.Add((false, "no successful page under mixed load"));
            }

            return validation;
        }, cancellationToken)).ToArray();

        await writer;
        var results = (await Task.WhenAll(readerTasks)).SelectMany(result => result).ToList();
        await _coordinator.ReconcileAfterTransitionAsync(cancellationToken);
        return results;
    }

    private static bool SameRows(
        IReadOnlyCollection<OrderRow> left,
        IReadOnlyCollection<OrderRow> right)
    {
        if (left.Count != right.Count)
        {
            return false;
        }

        var rightById = right.ToDictionary(row => row.OrderId);
        return left.All(row =>
            rightById.TryGetValue(row.OrderId, out var other) && row == other);
    }

    private static void Add(List<SelfTestCheck> checks, string name, bool passed, string detail) =>
        checks.Add(new SelfTestCheck(name, passed, detail));

    private static double P95(IEnumerable<double> values)
    {
        var sorted = values.OrderBy(value => value).ToArray();
        if (sorted.Length == 0)
        {
            return 0;
        }

        var rank = (int)Math.Ceiling(sorted.Length * 0.95) - 1;
        return Math.Round(sorted[Math.Clamp(rank, 0, sorted.Length - 1)], 3);
    }
}
