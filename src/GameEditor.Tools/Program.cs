using System.Numerics;
using GameEditor.Spatial;

namespace GameEditor.Tools;

public static class Program
{
    public static int Main(string[] args)
    {
        var command = args.Length == 0 ? "help" : args[0].ToLowerInvariant();
        return command switch
        {
            "generate" => Generate(args),
            "demo" => Demo(),
            "verify" => Verify(),
            "help" or "--help" or "-h" => Help(),
            _ => Unknown(command)
        };
    }

    private static int Generate(string[] args)
    {
        var root = WorkspaceLocator.FindRoot();
        var output = args.Length > 1 ? Path.GetFullPath(args[1]) : Path.Combine(root, "samples", "default");
        var sample = DeterministicSceneGenerator.Generate();
        var write = DeterministicSceneGenerator.Write(sample, output);
        if (!write.IsSuccess)
        {
            Console.Error.WriteLine(write.Error);
            return 2;
        }

        Console.WriteLine($"Generated {sample.Scene.Nodes.Count} nodes, {sample.Scene.Geometries.Count} geometries.");
        Console.WriteLine($"Scene content hash: {sample.Scene.Declaration.ContentHash}");
        Console.WriteLine($"Final edit hash:    {sample.EditTruth.FinalContentHash}");
        Console.WriteLine($"Output:             {output}");
        return 0;
    }

    private static int Demo()
    {
        var paths = ResolveSamplePaths();
        var sceneResult = SceneJsonCodec.LoadScene(paths.Scene);
        if (!sceneResult.IsSuccess)
        {
            Console.Error.WriteLine($"Scene load failed: {sceneResult.Error}");
            return 2;
        }

        var scene = sceneResult.Value!;
        var queriesResult = SceneJsonCodec.LoadQueries(paths.Queries, scene);
        if (!queriesResult.IsSuccess)
        {
            Console.Error.WriteLine($"Query load failed: {queriesResult.Error}");
            return 2;
        }

        var truthResult = SceneJsonCodec.LoadTruth(paths.GroundTruth, queriesResult.Value!, scene);
        if (!truthResult.IsSuccess)
        {
            Console.Error.WriteLine($"Truth load failed: {truthResult.Error}");
            return 2;
        }

        var runtimeResult = SpatialRuntime.Create(scene);
        if (!runtimeResult.IsSuccess)
        {
            Console.Error.WriteLine($"Runtime creation failed: {runtimeResult.Error}");
            return 2;
        }

        var runtime = runtimeResult.Value!;
        Console.WriteLine($"Scene: {scene.Nodes.Count} nodes, {scene.Geometries.Count} geometries");
        Console.WriteLine($"Index: {runtime.CaptureSnapshot().IndexNodes.Count} hierarchy nodes");
        foreach (var ray in queriesResult.Value!.Rays)
        {
            var hit = runtime.Raycast(ray);
            Console.WriteLine($"  {ray.Id,-20} {(hit.Value is null ? "<miss>" : $"{hit.Value.NodeId}/{hit.Value.GeometryId} t={SpatialMath.F(hit.Value.Distance)}")}");
        }

        foreach (var box in queriesResult.Value.Boxes)
        {
            var selected = runtime.SelectBox(box);
            Console.WriteLine($"  {box.Id,-20} [{string.Join(", ", selected.Value!)}]");
        }

        var editsResult = SceneJsonCodec.LoadEdits(paths.Edits);
        var editTruthResult = SceneJsonCodec.LoadEditTruth(paths.EditTruth);
        if (!editsResult.IsSuccess || !editTruthResult.IsSuccess)
        {
            Console.Error.WriteLine(editsResult.Error ?? editTruthResult.Error);
            return 2;
        }

        var applied = EditSequenceRunner.Apply(runtime, editsResult.Value!.Edits);
        if (!applied.IsSuccess)
        {
            Console.Error.WriteLine($"Edit apply failed: {applied.Error}");
            return 2;
        }

        foreach (var step in applied.Value!)
        {
            Console.WriteLine(
                $"  {step.EditId,-28} rev={step.Result.Revision} " +
                $"rebuilt={step.Result.Stats.RebuiltNodes} reused={step.Result.Stats.ReusedNodes} " +
                $"invalidations={step.Result.Invalidations.Count}");
        }

        var full = runtime.CreateFullRebuild();
        if (!full.IsSuccess)
        {
            Console.Error.WriteLine($"Full rebuild failed: {full.Error}");
            return 2;
        }

        var comparison = EquivalenceChecker.Compare(
            runtime,
            full.Value!,
            queriesResult.Value.Rays,
            queriesResult.Value.Boxes);
        Console.WriteLine(comparison.ToSummary());

        var snapshotPath = Path.Combine(WorkspaceLocator.FindRoot(), "artifacts", "demo-snapshot.json");
        var saved = SnapshotStore.Save(runtime, snapshotPath);
        if (!saved.IsSuccess)
        {
            Console.Error.WriteLine($"Snapshot save failed: {saved.Error}");
            return 2;
        }

        var finalScenePath = Path.Combine(WorkspaceLocator.FindRoot(), "artifacts", "demo-final-scene.json");
        var sceneWrite = SceneJsonCodec.WriteJson(finalScenePath, runtime.ExportSceneDocument());
        if (!sceneWrite.IsSuccess)
        {
            Console.Error.WriteLine($"Final scene write failed: {sceneWrite.Error}");
            return 2;
        }

        var restartedScene = SceneJsonCodec.LoadScene(finalScenePath);
        if (!restartedScene.IsSuccess)
        {
            Console.Error.WriteLine($"Restart scene load failed: {restartedScene.Error}");
            return 2;
        }

        var shell = SpatialRuntime.CreateShell(restartedScene.Value!);
        if (!shell.IsSuccess)
        {
            Console.Error.WriteLine($"Shell creation failed: {shell.Error}");
            return 2;
        }

        var load = SnapshotStore.LoadInto(shell.Value!, snapshotPath);
        Console.WriteLine(load.IsSuccess
            ? $"Snapshot loaded: revision={load.Value!.Revision} hash={load.Value.SnapshotHash[..12]}..."
            : $"Snapshot rejected: {load.Error}");
        return comparison.IsEqual && load.IsSuccess ? 0 : 1;
    }

    private static int Verify()
    {
        var root = WorkspaceLocator.FindRoot();
        var paths = ResolveSamplePaths();
        if (!File.Exists(paths.Scene))
        {
            var sample = DeterministicSceneGenerator.Generate();
            var write = DeterministicSceneGenerator.Write(sample, Path.GetDirectoryName(paths.Scene)!);
            if (!write.IsSuccess)
            {
                Console.Error.WriteLine(write.Error);
                return 2;
            }
        }

        var scene = SceneJsonCodec.LoadScene(paths.Scene);
        var queries = scene.IsSuccess
            ? SceneJsonCodec.LoadQueries(paths.Queries, scene.Value!)
            : Result<QueryManifest>.Fail(scene.Error!.Code, scene.Error.Message, scene.Error.Path);
        if (!scene.IsSuccess || !queries.IsSuccess)
        {
            Console.Error.WriteLine(scene.Error ?? queries.Error);
            return 2;
        }

        var runtime = SpatialRuntime.Create(scene.Value!);
        if (!runtime.IsSuccess)
        {
            Console.Error.WriteLine(runtime.Error);
            return 2;
        }

        var loadedRuntime = runtime.Value!;
        var truth = SceneJsonCodec.LoadTruth(paths.GroundTruth, queries.Value!, scene.Value!);
        if (!truth.IsSuccess)
        {
            Console.Error.WriteLine(truth.Error);
            return 2;
        }

        var rayHits = 0;
        var truthMismatches = 0;
        foreach (var ray in queries.Value!.Rays)
        {
            var hit = loadedRuntime.Raycast(ray);
            if (hit.Value is not null)
            {
                rayHits++;
            }

            var expected = truth.Value!.Rays.Single(item => item.QueryId == ray.Id);
            var actualHit = hit.Value;
            var hitMatches = expected.Hit == (actualHit is not null);
            if (hitMatches && actualHit is not null)
            {
                hitMatches =
                    expected.NodeId == actualHit.NodeId &&
                    expected.GeometryId == actualHit.GeometryId &&
                    MathF.Abs(expected.Distance - actualHit.Distance) <= 1e-5f;
            }

            if (!hitMatches)
            {
                truthMismatches++;
            }
        }

        var boxHits = queries.Value.Boxes.Sum(box => loadedRuntime.SelectBox(box).Value!.Count);
        foreach (var box in queries.Value.Boxes)
        {
            var expected = truth.Value!.Boxes.Single(item => item.QueryId == box.Id);
            var actual = loadedRuntime.SelectBox(box).Value!;
            if (!expected.NodeIds.SequenceEqual(actual, StringComparer.Ordinal))
            {
                truthMismatches++;
            }
        }

        var valid = loadedRuntime.ValidateDerivedState();
        Console.WriteLine(valid.IsSuccess
            ? "Validation: derived cache versions are consistent."
            : $"Validation: {valid.Error}");
        Console.WriteLine($"Rays: {queries.Value.Rays.Count} ({rayHits} hits, {queries.Value.Rays.Count - rayHits} misses), truth mismatches: {truthMismatches}");
        Console.WriteLine($"Box selections: {queries.Value.Boxes.Count} queries, {boxHits} total node hits");

        var edits = SceneJsonCodec.LoadEdits(paths.Edits);
        var editTruth = SceneJsonCodec.LoadEditTruth(paths.EditTruth);
        if (!edits.IsSuccess || !editTruth.IsSuccess)
        {
            Console.Error.WriteLine(edits.Error ?? editTruth.Error);
            return 2;
        }

        var applied = EditSequenceRunner.Apply(loadedRuntime, edits.Value!.Edits);
        if (!applied.IsSuccess)
        {
            Console.Error.WriteLine(applied.Error);
            return 1;
        }

        var editMismatches = 0;
        if (loadedRuntime.ContentHash != editTruth.Value!.FinalContentHash)
        {
            editMismatches++;
        }

        var finalSnapshot = loadedRuntime.CaptureSnapshot();
        foreach (var expectedNode in editTruth.Value.FinalNodes)
        {
            var actualNode = finalSnapshot.Nodes.Single(node => node.NodeId == expectedNode.NodeId);
            var actualMatrix = SpatialMath.MatrixToRowMajor(actualNode.WorldTransform);
            if (expectedNode.WorldMatrix.Where((value, index) => MathF.Abs(value - actualMatrix[index]) > 1e-5f).Any())
            {
                editMismatches++;
            }

            if (expectedNode.BoundsEmpty != actualNode.WorldBounds.IsEmpty)
            {
                editMismatches++;
            }
            else if (!expectedNode.BoundsEmpty &&
                     (expectedNode.BoundsMin.Where((value, index) =>
                          MathF.Abs(value - actualNode.WorldBounds.Min[index]) > 1e-5f).Any() ||
                      expectedNode.BoundsMax.Where((value, index) =>
                          MathF.Abs(value - actualNode.WorldBounds.Max[index]) > 1e-5f).Any()))
            {
                editMismatches++;
            }
        }

        var full = loadedRuntime.CreateFullRebuild();
        if (!full.IsSuccess)
        {
            Console.Error.WriteLine(full.Error);
            return 1;
        }

        var comparison = EquivalenceChecker.Compare(
            loadedRuntime,
            full.Value!,
            queries.Value.Rays,
            queries.Value.Boxes);
        Console.WriteLine($"Edit truth mismatches: {editMismatches}");
        Console.WriteLine(comparison.ToSummary());
        Console.WriteLine($"Run the full acceptance suite with: dotnet run --project {Path.Combine(root, "tests", "GameEditor.Tests")}");
        return valid.IsSuccess && truthMismatches == 0 && editMismatches == 0 && comparison.IsEqual ? 0 : 1;
    }

    private static (string Scene, string Queries, string GroundTruth, string Edits, string EditTruth) ResolveSamplePaths()
    {
        var directory = Path.Combine(WorkspaceLocator.FindRoot(), "samples", "default");
        return (
            Path.Combine(directory, SampleFiles.Scene),
            Path.Combine(directory, SampleFiles.Queries),
            Path.Combine(directory, SampleFiles.GroundTruth),
            Path.Combine(directory, SampleFiles.Edits),
            Path.Combine(directory, SampleFiles.EditTruth));
    }

    private static int Help()
    {
        Console.WriteLine("GameEditor.Spatial sample tool");
        Console.WriteLine("  dotnet run --project src/GameEditor.Tools -- generate [outputDir]");
        Console.WriteLine("  dotnet run --project src/GameEditor.Tools -- demo");
        Console.WriteLine("  dotnet run --project src/GameEditor.Tools -- verify");
        return 0;
    }

    private static int Unknown(string command)
    {
        Console.Error.WriteLine($"Unknown command '{command}'.");
        return 1;
    }
}
