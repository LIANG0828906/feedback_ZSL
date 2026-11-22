namespace GameEditor.Tests;

public static class Program
{
    public static int Main()
    {
        var tests = new List<(string Name, Action Test)>
        {
            ("generator is deterministic and matches checked-in sample", AcceptanceTests.GeneratorDeterministic),
            ("schema, cycle, reference and finite-number validation", AcceptanceTests.StructuralValidation),
            ("ray and box truth, misses and query-order independence", AcceptanceTests.PickingTruth),
            ("frustum query returns visible nodes", AcceptanceTests.FrustumSelection),
            ("transform propagation reuses caches and invalidates precisely", AcceptanceTests.TransformPropagation),
            ("topology change and failed batch preserve state", AcceptanceTests.TopologyAndRollback),
            ("parent and child pivot selection moves only once", AcceptanceTests.PivotNoDoubleTransform),
            ("pivot rotation and scale keep their world pivot fixed", AcceptanceTests.PivotPivotInvariant),
            ("pivot sessions isolate, cancel and reject stale commits", AcceptanceTests.PivotIsolation),
            ("edit sequence matches analytic truth and full rebuild", AcceptanceTests.EditSequenceEquivalence),
            ("sequential and prepare-parallel edit order agree", AcceptanceTests.EditOrderDeterminism),
            ("divergence checker reports first object and keeps snapshots", AcceptanceTests.DivergenceReporting),
            ("snapshot round-trip restores all derived data", AcceptanceTests.SnapshotRoundTrip),
            ("snapshot version/hash/truncation rejection stays empty", AcceptanceTests.SnapshotRejection),
            ("snapshot writes are serial and atomic", AcceptanceTests.SnapshotAtomicity),
            ("single entry failure does not poison other entries", AcceptanceTests.EntryFailureIsolation),
            ("parallel queries and batch limits are deterministic", AcceptanceTests.ConcurrentQueries),
            ("cancellation before commit leaves runtime unchanged", AcceptanceTests.CancellationBoundary)
        };

        return TestRunner.Run(tests);
    }
}
