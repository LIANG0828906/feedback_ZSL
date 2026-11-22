using System.Numerics;
using System.Text.Json;
using System.Text.Json.Nodes;
using GameEditor.Spatial;

namespace GameEditor.Tests;

internal static class AcceptanceTests
{
    private const float Tolerance = 1e-5f;

    public static void GeneratorDeterministic()
    {
        var root = FindRoot();
        var first = DeterministicSceneGenerator.Generate();
        var second = DeterministicSceneGenerator.Generate();
        TestAssert.Equal(first.Scene.Declaration.ContentHash, second.Scene.Declaration.ContentHash, "scene hash");
        TestAssert.Equal(first.EditTruth.FinalContentHash, second.EditTruth.FinalContentHash, "edit hash");
        TestAssert.Equal(first.Scene.Nodes.Count, first.EditTruth.FinalNodes.Count, "node truth count");

        var tempA = ResetDirectory("generator-a");
        var tempB = ResetDirectory("generator-b");
        TestAssert.Success(DeterministicSceneGenerator.Write(first, tempA), "write first sample");
        TestAssert.Success(DeterministicSceneGenerator.Write(second, tempB), "write second sample");

        var fileNames = Directory.GetFiles(tempA).Select(Path.GetFileName).OrderBy(name => name).ToList();
        var otherNames = Directory.GetFiles(tempB).Select(Path.GetFileName).OrderBy(name => name).ToList();
        TestAssert.SequenceEqual(fileNames, otherNames, "generated file names");
        foreach (var name in fileNames)
        {
            var bytesA = File.ReadAllBytes(Path.Combine(tempA, name!));
            var bytesB = File.ReadAllBytes(Path.Combine(tempB, name!));
            TestAssert.Equal(bytesA.Length, bytesB.Length, $"byte length for {name}");
            TestAssert.True(bytesA.AsSpan().SequenceEqual(bytesB), $"deterministic bytes for {name}");
        }

        var checkedIn = Path.Combine(root, "samples", "default");
        foreach (var name in fileNames)
        {
            var generated = File.ReadAllBytes(Path.Combine(tempA, name!));
            var committed = File.ReadAllBytes(Path.Combine(checkedIn, name!));
            TestAssert.True(generated.AsSpan().SequenceEqual(committed), $"checked-in sample is current for {name}");
        }
    }

    public static void StructuralValidation()
    {
        var fixture = SampleFixture.Load();
        var sourceText = File.ReadAllText(fixture.ScenePath);
        var temp = ResetDirectory("schema");

        var missing = JsonNode.Parse(sourceText)!.AsObject();
        missing["nodes"]!.AsArray()[0]!.AsObject().Remove("parentId");
        var missingResult = LoadSceneFromNode(missing, Path.Combine(temp, "missing.json"));
        TestAssert.Error(ErrorCode.E_SCHEMA, missingResult.Error, "missing field");
        TestAssert.True(missingResult.Error!.Path!.Contains("$.nodes[0].parentId"), "missing field path");

        var cycle = JsonNode.Parse(sourceText)!.AsObject();
        cycle["nodes"]!.AsArray()[0]!.AsObject()["parentId"] = "left-mesh-tip";
        var cycleResult = LoadSceneFromNode(cycle, Path.Combine(temp, "cycle.json"));
        TestAssert.Error(ErrorCode.E_CYCLE, cycleResult.Error, "parent cycle");

        var badRef = JsonNode.Parse(sourceText)!.AsObject();
        badRef["geometries"]!.AsArray()[0]!.AsObject()["nodeId"] = "missing-node";
        var refResult = LoadSceneFromNode(badRef, Path.Combine(temp, "ref.json"));
        TestAssert.Error(ErrorCode.E_REF, refResult.Error, "node reference");
        TestAssert.True(refResult.Error!.Path!.Contains("nodeId"), "reference path");

        var meshIndex = JsonNode.Parse(sourceText)!.AsObject();
        var mesh = meshIndex["geometries"]!.AsArray()
            .First(item => item!["kind"]!.GetValue<string>() == "triangleMesh")!;
        mesh["indices"]!.AsArray()[0] = 9999;
        var meshResult = LoadSceneFromNode(meshIndex, Path.Combine(temp, "mesh.json"));
        TestAssert.Error(ErrorCode.E_REF, meshResult.Error, "mesh index reference");

        var nonFinite = JsonNode.Parse(sourceText)!.AsObject();
        nonFinite["nodes"]!.AsArray()[0]!.AsObject()["localMatrix"]!.AsArray()[0] = 1e40;
        var finiteResult = LoadSceneFromNode(nonFinite, Path.Combine(temp, "finite.json"));
        TestAssert.Error(ErrorCode.E_SCHEMA, finiteResult.Error, "non-finite transform");

        var hashMismatch = JsonNode.Parse(sourceText)!.AsObject();
        hashMismatch["declaration"]!.AsObject()["contentHash"] = new string('0', 64);
        var hashResult = LoadSceneFromNode(hashMismatch, Path.Combine(temp, "hash.json"));
        TestAssert.Error(ErrorCode.E_SCHEMA, hashResult.Error, "declaration mismatch");

        var direct = SampleFixture.CloneScene(fixture.Scene);
        direct.Nodes[0].LocalMatrix[0] = float.NaN;
        var directResult = SceneValidator.ValidateScene(direct);
        TestAssert.Error(ErrorCode.E_SCHEMA, directResult.Error, "NaN transform");
    }

    public static void PickingTruth()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        TestAssert.True(
            fixture.GroundTruth.Rays.Any(ray => ray.Hit),
            "sample must contain ray hits");
        TestAssert.True(
            fixture.GroundTruth.Rays.Any(ray => !ray.Hit),
            "sample must contain ray misses");

        foreach (var truth in fixture.GroundTruth.Rays)
        {
            var query = fixture.Queries.Rays.Single(item => item.Id == truth.QueryId);
            var result = runtime.Raycast(query);
            TestAssert.Success(result, $"ray {truth.QueryId}");
            if (!truth.Hit)
            {
                TestAssert.True(result.Value is null, $"ray {truth.QueryId} must be an explicit miss");
            }
            else
            {
                TestAssert.True(result.Value is not null, $"ray {truth.QueryId} must hit");
                TestAssert.Equal(truth.NodeId, result.Value!.NodeId, $"ray {truth.QueryId} node");
                TestAssert.Equal(truth.GeometryId, result.Value.GeometryId, $"ray {truth.QueryId} geometry");
                TestAssert.Close(truth.Distance, result.Value.Distance, Tolerance, $"ray {truth.QueryId} distance");
                TestAssert.VectorEqual(
                    SpatialMath.VectorFromArray(truth.Point!),
                    result.Value.Point,
                    Tolerance,
                    $"ray {truth.QueryId} point");

                var all = runtime.RaycastAll(query);
                TestAssert.Success(all, $"ray-all {truth.QueryId}");
                TestAssert.Equal(truth.AllHits.Count, all.Value!.Count, $"ray-all {truth.QueryId} count");
                for (var i = 0; i < truth.AllHits.Count; i++)
                {
                    TestAssert.Equal(truth.AllHits[i].GeometryId, all.Value[i].GeometryId, "all-hit geometry");
                    TestAssert.Equal(truth.AllHits[i].NodeId, all.Value[i].NodeId, "all-hit node");
                    TestAssert.Close(truth.AllHits[i].Distance, all.Value[i].Distance, Tolerance, "all-hit distance");
                }
            }
        }

        foreach (var truth in fixture.GroundTruth.Boxes)
        {
            var query = fixture.Queries.Boxes.Single(item => item.Id == truth.QueryId);
            var result = runtime.SelectBox(query);
            TestAssert.Success(result, $"box {truth.QueryId}");
            TestAssert.SequenceEqual(truth.NodeIds, result.Value!, $"box {truth.QueryId}");
        }

        var reverseRays = fixture.GroundTruth.Rays.AsEnumerable().Reverse().ToList();
        foreach (var truth in reverseRays)
        {
            var query = fixture.Queries.Rays.Single(item => item.Id == truth.QueryId);
            var result = runtime.Raycast(query);
            TestAssert.Equal(truth.NodeId, result.Value?.NodeId, $"reverse ray {truth.QueryId}");
        }

        foreach (var truth in fixture.GroundTruth.Boxes.AsEnumerable().Reverse())
        {
            var query = fixture.Queries.Boxes.Single(item => item.Id == truth.QueryId);
            var result = runtime.SelectBox(query);
            TestAssert.SequenceEqual(truth.NodeIds, result.Value!, $"reverse box {truth.QueryId}");
        }
    }

    public static void FrustumSelection()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var selection = runtime.SelectFrustum();
        TestAssert.Success(selection, "frustum selection");
        TestAssert.True(selection.Value!.NodeIds.Count > 0, "frustum must select visible nodes");
        TestAssert.True(selection.Value.NodeIds.Contains("center-crate"), "frustum includes center crate");
        var sorted = selection.Value.NodeIds.OrderBy(item => item, StringComparer.Ordinal).ToList();
        TestAssert.SequenceEqual(sorted, selection.Value.NodeIds, "frustum result is sorted");
    }

    public static void TransformPropagation()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();
        var local = runtime.GetLocalTransform("left-mesh-tip");
        TestAssert.Success(local, "read local transform");
        var updated = local.Value * Matrix4x4.CreateTranslation(0, 0.6f, 0);
        var applied = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["left-mesh-tip"] = updated
            });
        TestAssert.Success(applied, "leaf transform update");
        TestAssert.Equal(17, applied.Value!.Stats.ReusedNodes, "leaf update reuses non-descendant nodes");
        TestAssert.Equal(1, applied.Value.Stats.RebuiltNodes, "leaf update rebuilds one node");
        TestAssert.Equal(13, applied.Value.Stats.ReusedEntries, "leaf update reuses entries");

        var invalidationIds = applied.Value.Invalidations.Select(item => item.ObjectId).ToHashSet(StringComparer.Ordinal);
        TestAssert.True(invalidationIds.Contains("derived:world:left-mesh-tip"), "tip world invalidated");
        TestAssert.True(invalidationIds.Contains("derived:entry:g-left-mesh-tip"), "tip entry invalidated");
        TestAssert.True(invalidationIds.Contains("index:node:root"), "root index invalidated");
        TestAssert.False(invalidationIds.Contains("derived:world:left-mesh-a"), "ancestor world cache reused");
        TestAssert.False(invalidationIds.Contains("derived:entry:g-right-crate-a"), "unrelated entry untouched");

        var after = runtime.CaptureSnapshot();
        foreach (var beforeNode in before.Nodes)
        {
            var afterNode = after.Nodes.Single(item => item.NodeId == beforeNode.NodeId);
            if (beforeNode.NodeId == "left-mesh-tip")
            {
                TestAssert.NotEqual(beforeNode.TransformVersion, afterNode.TransformVersion, "tip version updated");
                TestAssert.False(MatricesEqual(beforeNode.WorldTransform, afterNode.WorldTransform), "tip world changed");
            }
            else
            {
                TestAssert.Equal(beforeNode.TransformVersion, afterNode.TransformVersion, $"version reused for {beforeNode.NodeId}");
                TestAssert.True(MatricesEqual(beforeNode.WorldTransform, afterNode.WorldTransform), $"world reused for {beforeNode.NodeId}");
            }
        }

        AssertEquivalentToFull(runtime, fixture);

        var groupLocal = runtime.GetLocalTransform("rig-left");
        TestAssert.Success(groupLocal, "read group local");
        var groupUpdate = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["rig-left"] = groupLocal.Value * Matrix4x4.CreateTranslation(0, 0.25f, 0)
            });
        TestAssert.Success(groupUpdate, "group transform update");
        TestAssert.Equal(5, groupUpdate.Value!.Stats.RebuiltNodes, "group updates node and four descendants");
        TestAssert.Equal(13, groupUpdate.Value.Stats.ReusedNodes, "group leaves unrelated nodes reused");
        var groupInvalidations = groupUpdate.Value.Invalidations.Select(item => item.ObjectId).ToHashSet(StringComparer.Ordinal);
        TestAssert.True(groupInvalidations.Contains("derived:world:left-mesh-tip"), "descendant world invalidated");
        TestAssert.False(groupInvalidations.Contains("derived:world:rig-right"), "sibling world cache reused");
        TestAssert.False(groupInvalidations.Contains("index:node:rig-right"), "sibling index cache reused");
        AssertEquivalentToFull(runtime, fixture);
    }

    public static void TopologyAndRollback()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();

        var reparent = runtime.Reparent("left-mesh-tip", "rig-right");
        TestAssert.Error(ErrorCode.E_TOPO, reparent.Error, "reparent");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "reparent preserves state");

        var local = runtime.GetLocalTransform("left-mesh-tip");
        var invalidBatch = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["left-mesh-tip"] = local.Value * Matrix4x4.CreateTranslation(1, 0, 0),
                ["missing-node"] = Matrix4x4.Identity
            });
        TestAssert.Error(ErrorCode.E_REF, invalidBatch.Error, "batch with missing node");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "failed batch rolls back");

        var duplicate = runtime.ApplyAddGeometry(new GeometryDefinition
        {
            Id = "g-left-crate-a",
            NodeId = "rig-left",
            Kind = "sphere",
            Center = [0, 0, 0],
            Radius = 1
        });
        TestAssert.Error(ErrorCode.E_SCHEMA, duplicate.Error, "duplicate geometry");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "duplicate geometry preserves state");

        var removeMissing = runtime.ApplyRemoveGeometry("missing-geometry");
        TestAssert.Error(ErrorCode.E_REF, removeMissing.Error, "remove missing geometry");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "remove missing preserves state");
    }

    public static void PivotNoDoubleTransform()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();
        var beforeParent = WorldOrigin(before, "rig-left");
        var beforeChild = WorldOrigin(before, "left-crate-a");
        var session = runtime.BeginPivotSession(
            ["rig-left", "left-crate-a"],
            new Vector3(-5.8f, 1, 0));
        TestAssert.Success(session, "begin parent-child pivot session");
        TestAssert.SequenceEqual(["rig-left"], session.Value!.SelectionRoots, "only topmost selected node is a root");
        TestAssert.Success(
            session.Value.Apply(PivotOperation.Translate(new Vector3(0, 1, 0))),
            "apply pivot translation");
        var commit = session.Value.Commit();
        TestAssert.Success(commit, "commit pivot translation");

        var after = runtime.CaptureSnapshot();
        var afterParent = WorldOrigin(after, "rig-left");
        var afterChild = WorldOrigin(after, "left-crate-a");
        TestAssert.VectorEqual(beforeParent + new Vector3(0, 1, 0), afterParent, Tolerance, "parent translation");
        TestAssert.VectorEqual(beforeChild + new Vector3(0, 1, 0), afterChild, Tolerance, "child moves exactly once");
        TestAssert.Close(1f, afterChild.Y - beforeChild.Y, Tolerance, "child delta is one unit");
        AssertEquivalentToFull(runtime, fixture);
    }

    public static void PivotPivotInvariant()
    {
        var fixture = SampleFixture.Load();
        var rotationRuntime = CreateRuntime(fixture.Scene);
        var pivot = new Vector3(5.8f, 2.0f, 0);
        var before = rotationRuntime.CaptureSnapshot();
        var beforeRight = before.Nodes.Single(item => item.NodeId == "rig-right");
        var inverse = Inverse(beforeRight.WorldTransform);
        var localPivot = Vector3.Transform(pivot, inverse);
        var beforeChildOrigin = WorldOrigin(before, "right-sphere-a");

        var rotateSession = rotationRuntime.BeginPivotSession(["rig-right", "right-sphere-a"], pivot);
        TestAssert.Success(rotateSession, "begin rotate session");
        TestAssert.Success(
            rotateSession.Value!.Apply(PivotOperation.Rotate(Vector3.UnitY, 30)),
            "apply rotate");
        TestAssert.Success(rotateSession.Value.Commit(), "commit rotate");
        var afterRotate = rotationRuntime.CaptureSnapshot();
        var afterRight = afterRotate.Nodes.Single(item => item.NodeId == "rig-right");
        TestAssert.VectorEqual(
            pivot,
            Vector3.Transform(localPivot, afterRight.WorldTransform),
            Tolerance,
            "rotation keeps pivot fixed");
        AssertEquivalentToFull(rotationRuntime, fixture);

        var scaleRuntime = CreateRuntime(fixture.Scene);
        var scaleSession = scaleRuntime.BeginPivotSession(["rig-right", "right-sphere-a"], pivot);
        TestAssert.Success(scaleSession, "begin scale session");
        TestAssert.Success(scaleSession.Value!.Apply(PivotOperation.Scale(1.4f)), "apply scale");
        TestAssert.Success(scaleSession.Value.Commit(), "commit scale");
        var afterScale = scaleRuntime.CaptureSnapshot();
        var afterScaleRight = afterScale.Nodes.Single(item => item.NodeId == "rig-right");
        TestAssert.VectorEqual(
            pivot,
            Vector3.Transform(localPivot, afterScaleRight.WorldTransform),
            Tolerance,
            "scale keeps pivot fixed");
        var afterChildOrigin = WorldOrigin(afterScale, "right-sphere-a");
        TestAssert.Close(
            (beforeChildOrigin - pivot).Length() * 1.4f,
            (afterChildOrigin - pivot).Length(),
            Tolerance,
            "selected child scales once, not twice");
        AssertEquivalentToFull(scaleRuntime, fixture);

        var nonUniformScene = SampleFixture.CloneScene(fixture.Scene);
        var rigRight = nonUniformScene.Nodes.Single(node => node.Id == "rig-right");
        rigRight.LocalMatrix = SpatialMath.MatrixToRowMajor(
            SpatialMath.ComposeTRS(
                new Vector3(5.8f, 0, 0),
                Quaternion.CreateFromAxisAngle(Vector3.UnitY, 7f * MathF.PI / 180f),
                new Vector3(1.6f, 0.75f, 1.2f)));
        SampleFixture.RecomputeDeclaration(nonUniformScene);
        TestAssert.Success(SceneValidator.ValidateScene(nonUniformScene), "non-uniform parent scene");
        var nonUniformRuntime = CreateRuntime(nonUniformScene);
        var nonUniformBefore = nonUniformRuntime.CaptureSnapshot();
        var childWorld = nonUniformBefore.Nodes.Single(node => node.NodeId == "right-crate-a").WorldTransform;
        var nonUniformPivot = new Vector3(5.8f, 1.4f, 0.4f);
        var childPivotLocal = Vector3.Transform(nonUniformPivot, Inverse(childWorld));
        var nonUniformSession = nonUniformRuntime.BeginPivotSession(["right-crate-a"], nonUniformPivot);
        TestAssert.Success(nonUniformSession, "begin non-uniform parent session");
        TestAssert.Success(
            nonUniformSession.Value!.Apply(PivotOperation.Rotate(Vector3.UnitY, 23)),
            "apply non-uniform parent rotation");
        TestAssert.Success(nonUniformSession.Value.Commit(), "commit non-uniform parent rotation");
        var nonUniformAfter = nonUniformRuntime.CaptureSnapshot();
        var afterChildWorld = nonUniformAfter.Nodes.Single(node => node.NodeId == "right-crate-a").WorldTransform;
        var expectedWorld =
            childWorld *
            Matrix4x4.CreateTranslation(-nonUniformPivot) *
            Matrix4x4.CreateFromAxisAngle(Vector3.UnitY, 23f * MathF.PI / 180f) *
            Matrix4x4.CreateTranslation(nonUniformPivot);
        TestAssert.True(
            MatricesClose(expectedWorld, afterChildWorld, 1e-4f),
            "nested pivot world transform matches direct world-space rotation");
        TestAssert.VectorEqual(
            nonUniformPivot,
            Vector3.Transform(childPivotLocal, afterChildWorld),
            Tolerance,
            "world pivot fixed under non-uniform parent scale");
        AssertEquivalentToFull(nonUniformRuntime, fixture);
    }

    public static void PivotIsolation()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();
        var session = runtime.BeginPivotSession(["rig-center"], new Vector3(0, 1.5f, 0));
        TestAssert.Success(session, "begin isolated session");
        TestAssert.Success(session.Value!.Apply(PivotOperation.Translate(new Vector3(2, 0, 0))), "working-copy edit");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "uncommitted edit does not affect runtime");
        session.Value.Cancel();
        TestAssert.Error(
            ErrorCode.E_INVALID_OP,
            session.Value.Commit().Error,
            "canceled session cannot commit");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "canceled session leaves state");

        var stale = runtime.BeginPivotSession(["rig-left"], new Vector3(-5.8f, 1, 0));
        TestAssert.Success(stale, "begin stale session");
        TestAssert.Success(stale.Value!.Apply(PivotOperation.Translate(new Vector3(0, 3, 0))), "stale working edit");

        var centerLocal = runtime.GetLocalTransform("center-mesh");
        var unrelated = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["center-mesh"] = centerLocal.Value * Matrix4x4.CreateTranslation(0.1f, 0, 0)
            });
        TestAssert.Success(unrelated, "unrelated committed edit");
        var afterUnrelated = runtime.CaptureSnapshot();
        var staleCommit = stale.Value.Commit();
        TestAssert.Error(ErrorCode.E_STALE, staleCommit.Error, "stale pivot commit");
        AssertSnapshotValuesEqual(afterUnrelated, runtime.CaptureSnapshot(), "stale commit does not overwrite newer edit");
    }

    public static void EditSequenceEquivalence()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        foreach (var (edit, stepTruth) in fixture.Edits.Edits.Zip(fixture.EditTruth.Steps))
        {
            var applied = EditSequenceRunner.Apply(runtime, [edit]);
            TestAssert.Success(applied, $"apply edit {edit.Id}");
            TestAssert.Equal(stepTruth.ContentHash, runtime.ContentHash, $"content hash after {edit.Id}");
            AssertNodeTruth(runtime.CaptureSnapshot(), stepTruth.Nodes, $"truth after {edit.Id}");
            AssertEquivalentToFull(runtime, fixture);
        }

        TestAssert.Equal(fixture.EditTruth.FinalContentHash, runtime.ContentHash, "final content hash");
        AssertNodeTruth(runtime.CaptureSnapshot(), fixture.EditTruth.FinalNodes, "final node truth");
        AssertRayTruth(runtime, fixture.Queries.Rays, fixture.EditTruth.FinalRays, "final rays");
        AssertBoxTruth(runtime, fixture.Queries.Boxes, fixture.EditTruth.FinalBoxes, "final boxes");
        AssertEquivalentToFull(runtime, fixture);
    }

    public static void EditOrderDeterminism()
    {
        var fixture = SampleFixture.Load();
        var sequential = CreateRuntime(fixture.Scene);
        var prepared = CreateRuntime(fixture.Scene);
        var first = EditSequenceRunner.Apply(
            sequential,
            fixture.Edits.Edits,
            EditPreparationMode.Sequential);
        var second = EditSequenceRunner.Apply(
            prepared,
            fixture.Edits.Edits,
            EditPreparationMode.PrepareParallel);
        TestAssert.Success(first, "sequential edits");
        TestAssert.Success(second, "prepare-parallel edits");
        TestAssert.Equal(
            string.Join(",", first.Value!.Select(item => item.Result.ContentHash)),
            string.Join(",", second.Value!.Select(item => item.Result.ContentHash)),
            "edit effective order");
        TestAssert.Equal(sequential.ContentHash, prepared.ContentHash, "final content hash");
        AssertSnapshotValuesEqual(sequential.CaptureSnapshot(), prepared.CaptureSnapshot(), "edit path snapshots");
        AssertEquivalentToFull(sequential, fixture);
        AssertEquivalentToFull(prepared, fixture);
    }

    public static void DivergenceReporting()
    {
        var fixture = SampleFixture.Load();
        var left = CreateRuntime(fixture.Scene);
        var right = CreateRuntime(fixture.Scene);
        var leftLocal = left.GetLocalTransform("center-mesh");
        var rightLocal = right.GetLocalTransform("center-sphere");
        TestAssert.Success(
            left.ApplyTransformBatch(new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["center-mesh"] = leftLocal.Value * Matrix4x4.CreateTranslation(0.5f, 0, 0)
            }),
            "left divergence edit");
        TestAssert.Success(
            right.ApplyTransformBatch(new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["center-sphere"] = rightLocal.Value * Matrix4x4.CreateTranslation(0, 0.5f, 0)
            }),
            "right divergence edit");

        var report = EquivalenceChecker.Compare(left, right, fixture.Queries.Rays, fixture.Queries.Boxes);
        TestAssert.False(report.IsEqual, "different states must diverge");
        TestAssert.Error(ErrorCode.E_DIVERGE, report.Error, "divergence error");
        TestAssert.True(report.Divergence is not null, "divergence record");
        TestAssert.True(report.Divergence!.ObjectId.StartsWith("center-"), "first diverging object id");
        TestAssert.True(report.Divergence.Field.Contains("worldMatrix"), "diverging data item");
        TestAssert.True(report.IncrementalSnapshot.Nodes.Count > 0, "incremental snapshot retained");
        TestAssert.True(report.FullRebuildSnapshot.Nodes.Count > 0, "right snapshot retained");
    }

    public static void SnapshotRoundTrip()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var path = Path.Combine(ResetDirectory("snapshot-roundtrip"), "snapshot.json");
        var save = SnapshotStore.Save(runtime, path);
        TestAssert.Success(save, "save snapshot");
        var shell = SpatialRuntime.CreateShell(fixture.Scene);
        TestAssert.Success(shell, "snapshot shell");
        var load = SnapshotStore.LoadInto(shell.Value!, path);
        TestAssert.Success(load, "load snapshot");
        TestAssert.True(shell.Value!.IsInitialized, "shell initialized from snapshot");
        AssertSnapshotsExactly(runtime.CaptureSnapshot(), load.Value!.Restored, "snapshot round-trip");
        AssertRuntimeQueriesEqual(runtime, shell.Value, fixture);

        var opened = SnapshotStore.LoadOrRebuild(fixture.Scene, null, path);
        TestAssert.Success(opened, "load-or-rebuild valid snapshot");
        TestAssert.True(opened.Value!.UsedSnapshot, "valid snapshot is actually used");
        TestAssert.True(opened.Value.SnapshotError is null, "valid snapshot has no load error");

        var edited = CreateRuntime(fixture.Scene);
        TestAssert.Success(EditSequenceRunner.Apply(edited, fixture.Edits.Edits), "edited snapshot state");
        var editedPath = Path.Combine(Path.GetDirectoryName(path)!, "edited-snapshot.json");
        TestAssert.Success(SnapshotStore.Save(edited, editedPath), "save edited snapshot");
        var editedScenePath = Path.Combine(Path.GetDirectoryName(path)!, "edited-scene.json");
        TestAssert.Success(
            SceneJsonCodec.WriteJson(editedScenePath, edited.ExportSceneDocument()),
            "persist edited scene for restart");
        var reloadedScene = SceneJsonCodec.LoadScene(editedScenePath);
        TestAssert.Success(reloadedScene, "reload edited scene for restart");
        var editedShell = SpatialRuntime.CreateShell(reloadedScene.Value!);
        TestAssert.Success(editedShell, "edited shell");
        var editedLoad = SnapshotStore.LoadInto(editedShell.Value!, editedPath);
        TestAssert.Success(editedLoad, "load edited snapshot");
        AssertSnapshotsExactly(edited.CaptureSnapshot(), editedLoad.Value!.Restored, "edited snapshot round-trip");
        AssertRuntimeQueriesEqual(edited, editedShell.Value!, fixture);
    }

    public static void SnapshotRejection()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var dir = ResetDirectory("snapshot-rejection");
        var validPath = Path.Combine(dir, "valid.json");
        TestAssert.Success(SnapshotStore.Save(runtime, validPath), "save valid snapshot");
        var original = File.ReadAllText(validPath);

        AssertRejectedSnapshot(fixture.Scene, Tamper(original, root => root["contentHash"] = "deadbeef"), "content hash", dir, "bad-content.json");
        AssertRejectedSnapshot(fixture.Scene, Tamper(original, root => root["parameterVersion"] = "wrong-version"), "parameter version", dir, "bad-param.json");
        AssertRejectedSnapshot(fixture.Scene, Tamper(original, root =>
        {
            root["nodes"]!.AsArray()[0]!.AsObject()["worldMatrix"]!.AsArray()[0] = 12345;
        }), "snapshot hash", dir, "bad-derived.json");

        var truncated = Path.Combine(dir, "truncated.json");
        File.WriteAllText(truncated, "{");
        var truncatedShell = SpatialRuntime.CreateShell(fixture.Scene);
        TestAssert.Success(truncatedShell, "truncated shell");
        var truncatedLoad = SnapshotStore.LoadInto(truncatedShell.Value!, truncated);
        TestAssert.Error(ErrorCode.E_SNAP, truncatedLoad.Error, "truncated snapshot");
        TestAssert.False(truncatedShell.Value!.IsInitialized, "truncated snapshot leaves empty shell");

        var differentScene = SampleFixture.CloneScene(fixture.Scene);
        var local = SpatialMath.MatrixFromRowMajor(differentScene.Nodes.Single(node => node.Id == "center-mesh").LocalMatrix);
        differentScene.Nodes.Single(node => node.Id == "center-mesh").LocalMatrix =
            SpatialMath.MatrixToRowMajor(local * Matrix4x4.CreateTranslation(1, 0, 0));
        SampleFixture.RecomputeDeclaration(differentScene);
        TestAssert.Success(SceneValidator.ValidateScene(differentScene), "different scene validation");
        var differentShell = SpatialRuntime.CreateShell(differentScene);
        TestAssert.Success(differentShell, "different scene shell");
        var differentLoad = SnapshotStore.LoadInto(differentShell.Value!, validPath);
        TestAssert.Error(ErrorCode.E_SNAP, differentLoad.Error, "scene content mismatch");
        TestAssert.False(differentShell.Value!.IsInitialized, "rejected snapshot leaves empty state");
        TestAssert.Success(differentShell.Value.FullRebuild(), "explicit full rebuild after rejection");
        TestAssert.True(differentShell.Value.IsInitialized, "recovery runtime initialized");

        var recovered = SnapshotStore.LoadOrRebuild(differentScene, null, validPath);
        TestAssert.Success(recovered, "load-or-rebuild recovery");
        TestAssert.False(recovered.Value!.UsedSnapshot, "mismatched snapshot is discarded");
        TestAssert.Error(ErrorCode.E_SNAP, recovered.Value.SnapshotError, "recovery reports E_SNAP");
        TestAssert.True(recovered.Value.Runtime.IsInitialized, "recovery fully rebuilds");
    }

    public static void SnapshotAtomicity()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var dir = ResetDirectory("snapshot-atomic");
        var path = Path.Combine(dir, "snapshot.json");
        TestAssert.Success(SnapshotStore.Save(runtime, path), "initial snapshot save");
        File.WriteAllText(Path.Combine(dir, "orphan.tmp"), "garbage");

        var tasks = Enumerable.Range(0, 8)
            .Select(_ => Task.Run(() => SnapshotStore.Save(runtime, path)))
            .ToArray();
        Task.WaitAll(tasks);
        foreach (var task in tasks)
        {
            TestAssert.Success(task.Result, "concurrent snapshot save");
        }

        var shell = SpatialRuntime.CreateShell(fixture.Scene);
        TestAssert.Success(shell, "atomic load shell");
        var load = SnapshotStore.LoadInto(shell.Value!, path);
        TestAssert.Success(load, "snapshot after concurrent writes");
        AssertSnapshotsExactly(runtime.CaptureSnapshot(), load.Value!.Restored, "atomic snapshot content");
        TestAssert.False(
            Directory.GetFiles(dir, ".*.tmp").Any(),
            "successful saves leave no temp files");

        var cancel = new CancellationTokenSource();
        cancel.Cancel();
        var canceled = SnapshotStore.Save(runtime, path, cancel.Token);
        TestAssert.Error(ErrorCode.E_CANCELED, canceled.Error, "canceled snapshot save");
        var afterCancel = SpatialRuntime.CreateShell(fixture.Scene);
        TestAssert.Success(afterCancel, "after cancel shell");
        TestAssert.Success(SnapshotStore.LoadInto(afterCancel.Value!, path), "existing snapshot survives cancellation");
    }

    public static void EntryFailureIsolation()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();
        var singular = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["center-mesh"] = new Matrix4x4()
            });
        TestAssert.Success(singular, "singular transform rebuild is itemized");
        var after = runtime.CaptureSnapshot();
        var failedEntry = after.Entries.Single(entry => entry.GeometryId == "g-center-mesh");
        TestAssert.False(failedEntry.IsReady, "singular entry marked failed");
        TestAssert.True(failedEntry.Failure is not null, "failure reason retained");
        TestAssert.True(
            runtime.ValidateDerivedState().IsSuccess,
            "failed entry does not invalidate runtime");
        foreach (var oldEntry in before.Entries.Where(entry => entry.GeometryId != "g-center-mesh"))
        {
            var newEntry = after.Entries.Single(entry => entry.GeometryId == oldEntry.GeometryId);
            TestAssert.True(newEntry.IsReady, $"other entry ready {oldEntry.GeometryId}");
            TestAssert.Equal(oldEntry.CacheVersion, newEntry.CacheVersion, $"other entry reused {oldEntry.GeometryId}");
        }

        var ray = runtime.Raycast(fixture.Queries.Rays.Single(item => item.Id == "ray-center"));
        TestAssert.Success(ray, "query with isolated failed entry");
        AssertEquivalentToFull(runtime, fixture);
        var retry = runtime.RetryFailedEntries();
        TestAssert.Success(retry, "retry failed entries");
        TestAssert.True(retry.Value, "retry attempted");
        TestAssert.False(
            runtime.CaptureSnapshot().Entries.Single(entry => entry.GeometryId == "g-center-mesh").IsReady,
            "retry remains failed while transform is singular");

        var originalLocal = fixture.Scene.Nodes.Single(node => node.Id == "center-mesh").LocalMatrix;
        var repaired = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["center-mesh"] = SpatialMath.MatrixFromRowMajor(originalLocal)
            });
        TestAssert.Success(repaired, "repair transform");
        TestAssert.True(
            runtime.CaptureSnapshot().Entries.Single(entry => entry.GeometryId == "g-center-mesh").IsReady,
            "entry recovers after repair");
        AssertEquivalentToFull(runtime, fixture);
    }

    public static void ConcurrentQueries()
    {
        var fixture = SampleFixture.Load();
        var options = new SpatialRuntimeOptions
        {
            MaxDegreeOfParallelism = 2,
            MaxQueryBatchSize = 64,
            MaxCandidateBuffer = 4096
        };
        var runtime = CreateRuntime(fixture.Scene, options);
        var sequentialRays = runtime.RaycastBatch(fixture.Queries.Rays, parallel: false);
        var parallelRays = runtime.RaycastBatch(fixture.Queries.Rays, parallel: true);
        TestAssert.Success(sequentialRays, "sequential ray batch");
        TestAssert.Success(parallelRays, "parallel ray batch");
        TestAssert.Equal(sequentialRays.Value!.Count, parallelRays.Value!.Count, "ray batch count");
        for (var i = 0; i < sequentialRays.Value.Count; i++)
        {
            TestAssert.Equal(sequentialRays.Value[i]?.NodeId, parallelRays.Value[i]?.NodeId, $"ray batch {i} node");
            TestAssert.Equal(sequentialRays.Value[i]?.GeometryId, parallelRays.Value[i]?.GeometryId, $"ray batch {i} geometry");
            if (sequentialRays.Value[i] is { } left && parallelRays.Value[i] is { } right)
            {
                TestAssert.Close(left.Distance, right.Distance, 0, $"ray batch {i} exact distance");
            }
        }

        var sequentialBoxes = runtime.SelectBoxBatch(fixture.Queries.Boxes, parallel: false);
        var parallelBoxes = runtime.SelectBoxBatch(fixture.Queries.Boxes, parallel: true);
        TestAssert.Success(sequentialBoxes, "sequential box batch");
        TestAssert.Success(parallelBoxes, "parallel box batch");
        for (var i = 0; i < sequentialBoxes.Value!.Count; i++)
        {
            TestAssert.SequenceEqual(
                sequentialBoxes.Value[i],
                parallelBoxes.Value![i],
                $"parallel box {i}");
        }

        var tasks = Enumerable.Range(0, 4)
            .Select(_ => Task.Run(() => runtime.RaycastBatch(fixture.Queries.Rays, parallel: true)))
            .ToArray();
        Task.WaitAll(tasks);
        foreach (var task in tasks)
        {
            TestAssert.Success(task.Result, "worker query batch");
            TestAssert.Equal(sequentialRays.Value[0]?.NodeId, task.Result.Value![0]?.NodeId, "worker query result");
        }

        var limited = CreateRuntime(
            fixture.Scene,
            new SpatialRuntimeOptions { MaxDegreeOfParallelism = 2, MaxQueryBatchSize = 2 });
        var limitedResult = limited.RaycastBatch(fixture.Queries.Rays, parallel: true);
        TestAssert.Error(ErrorCode.E_LIMIT, limitedResult.Error, "query batch limit");
    }

    public static void CancellationBoundary()
    {
        var fixture = SampleFixture.Load();
        var runtime = CreateRuntime(fixture.Scene);
        var before = runtime.CaptureSnapshot();
        var local = runtime.GetLocalTransform("rig-left");
        using var cancel = new CancellationTokenSource();
        cancel.Cancel();
        var result = runtime.ApplyTransformBatch(
            new Dictionary<string, Matrix4x4>(StringComparer.Ordinal)
            {
                ["rig-left"] = local.Value * Matrix4x4.CreateTranslation(5, 0, 0)
            },
            cancellationToken: cancel.Token);
        TestAssert.Error(ErrorCode.E_CANCELED, result.Error, "canceled transform batch");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "canceled batch leaves state");

        var session = runtime.BeginPivotSession(["rig-center"], new Vector3(0, 1.5f, 0));
        TestAssert.Success(session, "cancel session create");
        TestAssert.Success(session.Value!.Apply(PivotOperation.Translate(Vector3.UnitX)), "cancel session apply");
        var commit = session.Value.Commit(cancel.Token);
        TestAssert.Error(ErrorCode.E_CANCELED, commit.Error, "canceled session commit");
        TestAssert.False(session.Value.IsCommitted, "canceled session is not committed");
        AssertSnapshotValuesEqual(before, runtime.CaptureSnapshot(), "canceled commit leaves state");

        var query = runtime.Raycast(fixture.Queries.Rays[0], cancel.Token);
        TestAssert.Error(ErrorCode.E_CANCELED, query.Error, "canceled query");
    }

    private static SpatialRuntime CreateRuntime(
        SceneDocument scene,
        SpatialRuntimeOptions? options = null)
    {
        var result = SpatialRuntime.Create(scene, options);
        TestAssert.Success(result, "runtime creation");
        return result.Value!;
    }

    private static void AssertEquivalentToFull(SpatialRuntime runtime, SampleFixture fixture)
    {
        var full = runtime.CreateFullRebuild();
        TestAssert.Success(full, "full rebuild");
        var report = EquivalenceChecker.Compare(runtime, full.Value!, fixture.Queries.Rays, fixture.Queries.Boxes);
        TestAssert.True(report.IsEqual, report.ToSummary());
        var validation = runtime.ValidateDerivedState();
        TestAssert.Success(validation, "derived state validation");
    }

    private static void AssertRuntimeQueriesEqual(
        SpatialRuntime expected,
        SpatialRuntime actual,
        SampleFixture fixture)
    {
        for (var i = 0; i < fixture.Queries.Rays.Count; i++)
        {
            var left = expected.Raycast(fixture.Queries.Rays[i]);
            var right = actual.Raycast(fixture.Queries.Rays[i]);
            TestAssert.Success(left, "expected ray");
            TestAssert.Success(right, "actual ray");
            TestAssert.Equal(left.Value?.NodeId, right.Value?.NodeId, $"snapshot ray {i} node");
            TestAssert.Equal(left.Value?.GeometryId, right.Value?.GeometryId, $"snapshot ray {i} geometry");
            if (left.Value is { } leftHit && right.Value is { } rightHit)
            {
                TestAssert.Equal(
                    BitConverter.SingleToInt32Bits(leftHit.Distance),
                    BitConverter.SingleToInt32Bits(rightHit.Distance),
                    $"snapshot ray {i} distance");
            }
        }

        foreach (var box in fixture.Queries.Boxes)
        {
            var left = expected.SelectBox(box);
            var right = actual.SelectBox(box);
            TestAssert.Success(left, "expected box");
            TestAssert.Success(right, "actual box");
            TestAssert.SequenceEqual(left.Value!, right.Value!, $"snapshot box {box.Id}");
        }
    }

    private static void AssertNodeTruth(
        RuntimeSnapshot snapshot,
        IReadOnlyList<NodeTransformTruth> expected,
        string message)
    {
        foreach (var node in expected)
        {
            var actual = snapshot.Nodes.Single(item => item.NodeId == node.NodeId);
            var matrix = SpatialMath.MatrixFromRowMajor(node.WorldMatrix);
            for (var i = 0; i < 16; i++)
            {
                var actualValue = SpatialMath.MatrixToRowMajor(actual.WorldTransform)[i];
                TestAssert.Close(node.WorldMatrix[i], actualValue, Tolerance, $"{message} {node.NodeId} matrix[{i}]");
            }

            TestAssert.Equal(node.BoundsEmpty, actual.WorldBounds.IsEmpty, $"{message} {node.NodeId} bounds empty");
            if (!node.BoundsEmpty)
            {
                TestAssert.VectorEqual(
                    SpatialMath.VectorFromArray(node.BoundsMin),
                    actual.WorldBounds.Min,
                    Tolerance,
                    $"{message} {node.NodeId} bounds min");
                TestAssert.VectorEqual(
                    SpatialMath.VectorFromArray(node.BoundsMax),
                    actual.WorldBounds.Max,
                    Tolerance,
                    $"{message} {node.NodeId} bounds max");
            }

            TestAssert.True(MatricesFinite(matrix), $"{message} truth matrix finite");
        }
    }

    private static void AssertRayTruth(
        SpatialRuntime runtime,
        IReadOnlyList<RayQueryDefinition> queries,
        IReadOnlyList<RayTruth> expected,
        string message)
    {
        foreach (var truth in expected)
        {
            var query = queries.Single(item => item.Id == truth.QueryId);
            var actual = runtime.RaycastAll(query);
            TestAssert.Success(actual, $"{message} {truth.QueryId}");
            TestAssert.Equal(truth.AllHits.Count, actual.Value!.Count, $"{message} {truth.QueryId} count");
            for (var i = 0; i < truth.AllHits.Count; i++)
            {
                TestAssert.Equal(truth.AllHits[i].NodeId, actual.Value[i].NodeId, $"{message} hit node");
                TestAssert.Equal(truth.AllHits[i].GeometryId, actual.Value[i].GeometryId, $"{message} hit geometry");
                TestAssert.Close(truth.AllHits[i].Distance, actual.Value[i].Distance, Tolerance, $"{message} hit distance");
                TestAssert.VectorEqual(
                    SpatialMath.VectorFromArray(truth.AllHits[i].Point),
                    actual.Value[i].Point,
                    Tolerance,
                    $"{message} hit point");
            }
        }
    }

    private static void AssertBoxTruth(
        SpatialRuntime runtime,
        IReadOnlyList<BoxQueryDefinition> queries,
        IReadOnlyList<BoxTruth> expected,
        string message)
    {
        foreach (var truth in expected)
        {
            var query = queries.Single(item => item.Id == truth.QueryId);
            var actual = runtime.SelectBox(query);
            TestAssert.Success(actual, $"{message} {truth.QueryId}");
            TestAssert.SequenceEqual(truth.NodeIds, actual.Value!, $"{message} {truth.QueryId}");
        }
    }

    private static void AssertSnapshotValuesEqual(
        RuntimeSnapshot expected,
        RuntimeSnapshot actual,
        string message)
    {
        TestAssert.Equal(expected.ContentHash, actual.ContentHash, $"{message} content hash");
        TestAssert.Equal(expected.StructureHash, actual.StructureHash, $"{message} structure hash");
        TestAssert.Equal(expected.Revision, actual.Revision, $"{message} revision");
        for (var i = 0; i < expected.Nodes.Count; i++)
        {
            TestAssert.Equal(expected.Nodes[i].NodeId, actual.Nodes[i].NodeId, $"{message} node id");
            TestAssert.True(
                MatricesEqual(expected.Nodes[i].WorldTransform, actual.Nodes[i].WorldTransform),
                $"{message} node matrix {expected.Nodes[i].NodeId}");
            TestAssert.Equal(expected.Nodes[i].WorldBounds, actual.Nodes[i].WorldBounds, $"{message} node bounds");
        }

        for (var i = 0; i < expected.Entries.Count; i++)
        {
            TestAssert.Equal(expected.Entries[i].GeometryId, actual.Entries[i].GeometryId, $"{message} entry id");
            TestAssert.Equal(expected.Entries[i].WorldBounds, actual.Entries[i].WorldBounds, $"{message} entry bounds");
            TestAssert.Equal(expected.Entries[i].IsReady, actual.Entries[i].IsReady, $"{message} entry ready");
        }

        for (var i = 0; i < expected.IndexNodes.Count; i++)
        {
            TestAssert.Equal(expected.IndexNodes[i].NodeId, actual.IndexNodes[i].NodeId, $"{message} index id");
            TestAssert.Equal(
                expected.IndexNodes[i].AggregateBounds,
                actual.IndexNodes[i].AggregateBounds,
                $"{message} index bounds");
        }
    }

    private static void AssertSnapshotsExactly(
        RuntimeSnapshot expected,
        RuntimeSnapshot actual,
        string message)
    {
        TestAssert.Equal(expected.ContentHash, actual.ContentHash, $"{message} content");
        TestAssert.Equal(expected.StructureHash, actual.StructureHash, $"{message} structure");
        TestAssert.Equal(expected.Revision, actual.Revision, $"{message} revision");
        AssertSnapshotValuesEqual(expected, actual, message);
        for (var i = 0; i < expected.Nodes.Count; i++)
        {
            TestAssert.Equal(
                expected.Nodes[i].TransformVersion,
                actual.Nodes[i].TransformVersion,
                $"{message} transform version");
            TestAssert.Equal(expected.Nodes[i].CacheVersion, actual.Nodes[i].CacheVersion, $"{message} node cache version");
        }

        for (var i = 0; i < expected.Entries.Count; i++)
        {
            TestAssert.Equal(expected.Entries[i].NodeTransformVersion, actual.Entries[i].NodeTransformVersion, $"{message} entry transform version");
            TestAssert.Equal(expected.Entries[i].CacheVersion, actual.Entries[i].CacheVersion, $"{message} entry cache version");
        }

        for (var i = 0; i < expected.IndexNodes.Count; i++)
        {
            TestAssert.Equal(expected.IndexNodes[i].CacheVersion, actual.IndexNodes[i].CacheVersion, $"{message} index cache version");
        }
    }

    private static void AssertRejectedSnapshot(
        SceneDocument scene,
        string json,
        string description,
        string directory,
        string fileName)
    {
        var path = Path.Combine(directory, fileName);
        File.WriteAllText(path, json);
        var shell = SpatialRuntime.CreateShell(scene);
        TestAssert.Success(shell, $"{description} shell");
        var load = SnapshotStore.LoadInto(shell.Value!, path);
        TestAssert.Error(ErrorCode.E_SNAP, load.Error, description);
        TestAssert.False(shell.Value!.IsInitialized, $"{description} leaves empty shell");
    }

    private static string Tamper(string json, Action<JsonNode> mutate)
    {
        var root = JsonNode.Parse(json)!;
        mutate(root);
        return root.ToJsonString(new JsonSerializerOptions { WriteIndented = true });
    }

    private static Result<SceneDocument> LoadSceneFromNode(JsonNode node, string path)
    {
        File.WriteAllText(path, node.ToJsonString(new JsonSerializerOptions { WriteIndented = true }));
        return SceneJsonCodec.LoadScene(path);
    }

    private static Matrix4x4 Inverse(Matrix4x4 matrix)
    {
        TestAssert.True(Matrix4x4.Invert(matrix, out var inverse), "matrix invertible");
        return inverse;
    }

    private static Vector3 WorldOrigin(RuntimeSnapshot snapshot, string nodeId)
    {
        var matrix = snapshot.Nodes.Single(item => item.NodeId == nodeId).WorldTransform;
        return new Vector3(matrix.M41, matrix.M42, matrix.M43);
    }

    private static bool MatricesEqual(Matrix4x4 left, Matrix4x4 right)
    {
        var leftValues = SpatialMath.MatrixToRowMajor(left);
        var rightValues = SpatialMath.MatrixToRowMajor(right);
        return leftValues.Select(BitConverter.SingleToInt32Bits)
            .SequenceEqual(rightValues.Select(BitConverter.SingleToInt32Bits));
    }

    private static bool MatricesFinite(Matrix4x4 matrix) => SpatialMath.IsFinite(matrix);

    private static bool MatricesClose(Matrix4x4 left, Matrix4x4 right, float tolerance)
    {
        var leftValues = SpatialMath.MatrixToRowMajor(left);
        var rightValues = SpatialMath.MatrixToRowMajor(right);
        for (var i = 0; i < 16; i++)
        {
            if (MathF.Abs(leftValues[i] - rightValues[i]) > tolerance)
            {
                return false;
            }
        }

        return true;
    }

    private static string ResetDirectory(string name)
    {
        var path = Path.Combine(FindRoot(), "artifacts", "test-tmp", name);
        if (Directory.Exists(path))
        {
            Directory.Delete(path, recursive: true);
        }

        Directory.CreateDirectory(path);
        return path;
    }

    private static string FindRoot()
    {
        var directory = new DirectoryInfo(Directory.GetCurrentDirectory());
        while (directory is not null)
        {
            if (File.Exists(Path.Combine(directory.FullName, "GameEditor.sln")))
            {
                return directory.FullName;
            }

            directory = directory.Parent;
        }

        throw new InvalidOperationException("Cannot locate workspace root.");
    }
}

internal sealed class SampleFixture
{
    private SampleFixture(
        string scenePath,
        SceneDocument scene,
        QueryManifest queries,
        GroundTruthManifest groundTruth,
        EditSequenceDefinition edits,
        EditTruthManifest editTruth)
    {
        ScenePath = scenePath;
        Scene = scene;
        Queries = queries;
        GroundTruth = groundTruth;
        Edits = edits;
        EditTruth = editTruth;
    }

    public string ScenePath { get; }
    public SceneDocument Scene { get; }
    public QueryManifest Queries { get; }
    public GroundTruthManifest GroundTruth { get; }
    public EditSequenceDefinition Edits { get; }
    public EditTruthManifest EditTruth { get; }

    public static SampleFixture Load()
    {
        var root = FindRoot();
        var directory = Path.Combine(root, "samples", "default");
        var scenePath = Path.Combine(directory, SampleFiles.Scene);
        if (!File.Exists(scenePath))
        {
            var generated = DeterministicSceneGenerator.Generate();
            var write = DeterministicSceneGenerator.Write(generated, directory);
            TestAssert.Success(write, "generate missing sample");
        }

        var sceneResult = SceneJsonCodec.LoadScene(scenePath);
        TestAssert.Success(sceneResult, "load scene");
        var queriesResult = SceneJsonCodec.LoadQueries(
            Path.Combine(directory, SampleFiles.Queries),
            sceneResult.Value!);
        TestAssert.Success(queriesResult, "load queries");
        var truthResult = SceneJsonCodec.LoadTruth(
            Path.Combine(directory, SampleFiles.GroundTruth),
            queriesResult.Value!,
            sceneResult.Value!);
        TestAssert.Success(truthResult, "load truth");
        var editsResult = SceneJsonCodec.LoadEdits(Path.Combine(directory, SampleFiles.Edits));
        TestAssert.Success(editsResult, "load edits");
        var editTruthResult = SceneJsonCodec.LoadEditTruth(Path.Combine(directory, SampleFiles.EditTruth));
        TestAssert.Success(editTruthResult, "load edit truth");
        return new SampleFixture(
            scenePath,
            sceneResult.Value!,
            queriesResult.Value!,
            truthResult.Value!,
            editsResult.Value!,
            editTruthResult.Value!);
    }

    public static SceneDocument CloneScene(SceneDocument scene)
    {
        var json = JsonSerializer.Serialize(scene, SceneJsonCodec.JsonOptions);
        var clone = JsonSerializer.Deserialize<SceneDocument>(json, SceneJsonCodec.JsonOptions)!;
        RecomputeDeclaration(clone);
        return clone;
    }

    public static void RecomputeDeclaration(SceneDocument scene)
    {
        var state = SceneState.FromDocument(scene);
        scene.Declaration = new SceneDeclaration
        {
            NodeCount = scene.Nodes.Count,
            GeometryCount = scene.Geometries.Count,
            ContentHash = state.ContentHash,
            StructureHash = state.StructureHash,
            ParamVersion = SpatialParameterVersion.Current
        };
    }

    private static string FindRoot()
    {
        var directory = new DirectoryInfo(Directory.GetCurrentDirectory());
        while (directory is not null)
        {
            if (File.Exists(Path.Combine(directory.FullName, "GameEditor.sln")))
            {
                return directory.FullName;
            }

            directory = directory.Parent;
        }

        throw new InvalidOperationException("Cannot locate workspace root.");
    }
}
