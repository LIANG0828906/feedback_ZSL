namespace OrderGateway.Scenarios;

public sealed record ScenarioResult(
    string Name,
    bool Passed,
    string Summary,
    IReadOnlyDictionary<string, object> Evidence);

public interface IScenario
{
    string Name { get; }
    Task<ScenarioResult> RunAsync(ScenarioContext context, CancellationToken cancellationToken);
}

public sealed class ScenarioContext
{
    public required GatewayOptions Options { get; init; }
    public required ConnectionGate Gate { get; init; }
    public required MetricsCollector Metrics { get; init; }
    public required OrderRepository Repository { get; init; }
    public required IndexLifecycleService Lifecycle { get; init; }
    public required OrderWorkload Workload { get; init; }
    public required IndexChangeBarrier IndexBarrier { get; init; }
    public required string ArtifactDirectory { get; init; }
}
