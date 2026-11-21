using System.Text;
using System.Text.Json;
using MeshRepair.Cli;
using MeshRepair.Core.Binary;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Recovery;
using MeshRepair.Core.Sample;

namespace MeshRepair.Tests;

public sealed class RecoveryAndDeterminismTests
{
    [Fact]
    public void LatestCorruptSnapshot_FallsBackToPreviousAndIgnoresTemp()
    {
        using var fixture = TestRoom.Create(withSnapshots: true);
        var v1 = fixture.Room.GetVersion(1).RequireValue();
        var session = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        Assert.True(session.WeldAcrossChunks(
            fixture.Pair,
            v1.Boundaries[fixture.Pair].ManifestVersion,
            fixture.Sample.WeldFirstVertexId,
            fixture.Sample.WeldSecondVertexId).IsSuccess);
        Assert.True(session.Submit().IsSuccess);
        var snapshots = Path.Combine(fixture.Directory, "snapshots");
        File.WriteAllText(
            Path.Combine(snapshots, "snapshot-0000000003.json.tmp"),
            "{\"partial\":true}",
            new UTF8Encoding(false));
        File.WriteAllText(
            Path.Combine(snapshots, "snapshot-0000000002.json"),
            "{not json",
            new UTF8Encoding(false));
        var loaded = fixture.Repository!.LoadLatest();
        Assert.True(loaded.IsSuccess, loaded.Error?.ToString());
        Assert.Equal(1, loaded.RequireValue().HeadVersion);
        Assert.Contains(2, loaded.RequireValue().SkippedSnapshots);
        Assert.Empty(loaded.RequireValue().RecomputedBlocks);
        Assert.True(File.Exists(Path.Combine(snapshots, "snapshot-0000000003.json.tmp")));
    }

    [Fact]
    public void MissingVersionField_ReturnsRecoveryInvalidWithVersionField()
    {
        var directory = Path.Combine(Path.GetTempPath(), $"mesh-repair-recovery-{Guid.NewGuid():N}");
        Directory.CreateDirectory(directory);
        try
        {
            var sample = MeshSampleGenerator.Generate();
            var layout = ChunkLayout.FromMesh(sample.Mesh);
            var baseline = MeshRepair.Core.Model.BaselineInfo.FromMesh(sample.Mesh);
            var document = new SnapshotDocument
            {
                Version = 0,
                Layout = SnapshotLayoutDto.FromLayout(layout),
                Baseline = SnapshotBaselineDto.FromBaseline(baseline)
            };
            document.SelfHash = document.ComputeSelfHash();
            File.WriteAllText(
                Path.Combine(directory, "snapshot-0000000001.json"),
                JsonSerializer.Serialize(document, SnapshotJson.Options),
                new UTF8Encoding(false));
            var repository = SnapshotRepository.LoadFromDirectory(directory).RequireValue();
            var loaded = repository.LoadLatest();
            Assert.False(loaded.IsSuccess);
            Assert.Equal(MeshErrorCode.E_RECOVERY_INVALID, loaded.Error!.Code);
            Assert.Equal("Version", loaded.Error.Field);
        }
        finally
        {
            Directory.Delete(directory, recursive: true);
        }
    }

    [Fact]
    public void FullWorkflow_IsByteDeterministicAndAllChecksPass()
    {
        var firstDirectory = Path.Combine(Path.GetTempPath(), $"mesh-repair-workflow-a-{Guid.NewGuid():N}");
        var secondDirectory = Path.Combine(Path.GetTempPath(), $"mesh-repair-workflow-b-{Guid.NewGuid():N}");
        try
        {
            var first = VerificationWorkflow.Run(firstDirectory, 4242);
            var second = VerificationWorkflow.Run(secondDirectory, 4242);
            Assert.Equal(first.TraceText, second.TraceText);
            Assert.Equal(first.VersionsText, second.VersionsText);
            Assert.Equal(
                Directory.GetFiles(Path.Combine(firstDirectory, "snapshots"), "snapshot-*.json")
                    .Order()
                    .Select(HashUtil.Sha256File),
                Directory.GetFiles(Path.Combine(secondDirectory, "snapshots"), "snapshot-*.json")
                    .Order()
                    .Select(HashUtil.Sha256File));
            var checks = first.Summary.Checks;
            Assert.True(checks.BinaryRoundTrip);
            Assert.True(checks.StaleHandleRejected);
            Assert.True(checks.StaleBoundaryRejected);
            Assert.True(checks.FillFailureAtomic);
            Assert.True(checks.BusyChunkRejected);
            Assert.True(checks.RepeatSubmitRejected);
            Assert.True(checks.SnapshotIsolationDuringStagedEdit);
            Assert.True(checks.DisjointSessionsMerged);
            Assert.True(checks.RollbackInvalidatedSession);
            Assert.True(checks.RollbackRecomputedBoundary);
            Assert.True(checks.FinalSpliceValid);
            Assert.True(checks.RecoveryMatchedPublished);
            Assert.Empty(first.Summary.Recovery.RecomputedBlocks);
            Assert.True(first.Summary.Recovery.PartialTemporaryFileIgnored);
        }
        finally
        {
            if (Directory.Exists(firstDirectory))
            {
                Directory.Delete(firstDirectory, recursive: true);
            }

            if (Directory.Exists(secondDirectory))
            {
                Directory.Delete(secondDirectory, recursive: true);
            }
        }
    }
}
