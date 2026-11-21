using BattleEconomy.Core;

namespace BattleEconomy.Verify;

internal static class CrashChild
{
    public static int Run(string[] args)
    {
        if (args.Length != 5)
        {
            Console.Error.WriteLine("usage: --crash-child <storeDir> <scenarioName> <pendingBatchNumber> <readyFile>");
            return 2;
        }

        var storeDirectory = args[1];
        var scenarioName = args[2];
        var pendingBatchNumber = int.Parse(args[3]);
        var readyFile = args[4];

        var spec = ScenarioCatalog.BuiltIns.Single(item => item.Name == scenarioName);
        var bundle = DeterministicScenarioGenerator.Generate(spec);
        var store = new EconomySnapshotStore(storeDirectory);
        var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store);
        if (!opened.Success || opened.Engine is null)
        {
            Console.Error.WriteLine($"{opened.Error?.Code}: {opened.Error?.Message}");
            return 3;
        }

        var engine = opened.Engine;
        var batch = bundle.Batches.Single(item => item.BatchNumber == pendingBatchNumber);
        var current = engine.QueryCurrent();
        store.SavePendingBatch(new RecoveryPendingBatch(
            pendingBatchNumber,
            batch.EconomicSteps,
            batch.Destructions,
            current.ContentHash));
        File.WriteAllText(readyFile, "READY\n");

        Thread.Sleep(Timeout.Infinite);
        return 0;
    }
}
