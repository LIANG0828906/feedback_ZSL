using MeshRepair.Core.Chunks;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Sample;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Tests;

public sealed class ConcurrencyAndBoundaryTests
{
    [Fact]
    public void ChunkAssignment_IsUniqueAndRecordsCrossedBasis()
    {
        using var fixture = TestRoom.Create();
        var version = fixture.Room.GetVersion(1).RequireValue();
        Assert.Equal(version.FaceCount, version.Assignments.Count);
        Assert.All(version.Assignments.Values, assignment =>
        {
            Assert.Contains(assignment.Owner, version.Chunks.Keys);
            Assert.Contains(assignment.Owner, assignment.CandidateChunks);
        });
        Assert.Contains(version.Assignments.Values, assignment => assignment.CandidateChunks.Count > 1);
        var manifest = version.Boundaries[fixture.Pair];
        Assert.Equal(version.Version, manifest.ManifestVersion);
        Assert.Equal(version.Version, manifest.ChunkAVersion);
        Assert.Equal(version.Version, manifest.ChunkBVersion);
        Assert.NotEmpty(manifest.SharedVertexIds);
        Assert.False(string.IsNullOrWhiteSpace(manifest.ContentHash));
    }

    [Fact]
    public void SnapshotHandle_IsImmutableUntilReleased()
    {
        using var fixture = TestRoom.Create();
        using var handle = fixture.Room.AcquireSnapshot().RequireValue();
        var before = handle.Read().RequireValue();
        var weld = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        var manifest = before.Boundaries[fixture.Pair];
        Assert.True(weld.WeldAcrossChunks(
            fixture.Pair,
            manifest.ManifestVersion,
            fixture.Sample.WeldFirstVertexId,
            fixture.Sample.WeldSecondVertexId).IsSuccess);
        Assert.True(weld.Submit().IsSuccess);
        Assert.Equal(1, handle.Read().RequireValue().Version);
        handle.Release();
        Assert.Equal(MeshErrorCode.E_STALE_HANDLE, handle.Read().Error!.Code);
    }

    [Fact]
    public void ChunkLease_RejectsBusyAndCancelPreservesPublishedVersion()
    {
        using var fixture = TestRoom.Create();
        var first = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A }).RequireValue();
        var busy = fixture.Room.TryBeginEdit("b", new[] { fixture.Pair.A });
        Assert.Equal(MeshErrorCode.E_BUSY, busy.Error!.Code);
        first.Cancel();
        var second = fixture.Room.TryBeginEdit("b", new[] { fixture.Pair.A }).RequireValue();
        Assert.True(second.RecomputeNormals(fixture.Pair.A).IsSuccess);
        second.Cancel();
        Assert.Equal(1, fixture.Room.CurrentVersion);
    }

    [Fact]
    public void CancellationToken_IsCheckedAtBoundaryCheckpoint()
    {
        using var fixture = TestRoom.Create();
        using var cancellation = new CancellationTokenSource();
        var session = fixture.Room.TryBeginEdit(
            "a",
            new[] { fixture.Pair.A },
            cancellationToken: cancellation.Token).RequireValue();
        cancellation.Cancel();
        var edit = session.RecomputeNormals(fixture.Pair.A);
        Assert.Equal(MeshErrorCode.E_CANCELLED, edit.Error!.Code);
        var submit = session.Submit();
        Assert.Equal(MeshErrorCode.E_CANCELLED, submit.Error!.Code);
        Assert.Equal(1, fixture.Room.CurrentVersion);
        var next = fixture.Room.TryBeginEdit("b", new[] { fixture.Pair.A });
        Assert.True(next.IsSuccess);
    }

    [Fact]
    public void DisjointSessions_CanCommitWithoutLosingEachOther()
    {
        using var fixture = TestRoom.Create();
        var a = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A }).RequireValue();
        var b = fixture.Room.TryBeginEdit("b", new[] { fixture.Pair.B }).RequireValue();
        Assert.True(a.RecomputeNormals(fixture.Pair.A).IsSuccess);
        Assert.True(b.RecomputeNormals(fixture.Pair.B).IsSuccess);
        var versionA = a.Submit().RequireValue();
        var versionB = b.Submit().RequireValue();
        Assert.Equal(versionA.Version + 1, versionB.Version);
        Assert.Equal(versionA.Chunks[fixture.Pair.A].ContentHash, versionB.Chunks[fixture.Pair.A].ContentHash);
        Assert.NotEqual(versionA.Chunks[fixture.Pair.B].ContentHash, versionB.Chunks[fixture.Pair.B].ContentHash);
    }

    [Fact]
    public async Task ParallelDisjointCommits_AreSerializedIntoCompleteVersions()
    {
        using var fixture = TestRoom.Create();
        var first = fixture.Room.TryBeginEdit("parallel-a", new[] { fixture.Pair.A }).RequireValue();
        var second = fixture.Room.TryBeginEdit("parallel-b", new[] { fixture.Pair.B }).RequireValue();
        using var barrier = new Barrier(3);
        var taskA = Task.Run(() =>
        {
            barrier.SignalAndWait();
            Assert.True(first.RecomputeNormals(fixture.Pair.A).IsSuccess);
            return first.Submit().RequireValue();
        });
        var taskB = Task.Run(() =>
        {
            barrier.SignalAndWait();
            Assert.True(second.RecomputeNormals(fixture.Pair.B).IsSuccess);
            return second.Submit().RequireValue();
        });
        barrier.SignalAndWait();
        var versions = await Task.WhenAll(taskA, taskB);
        Assert.Equal(new long[] { 2, 3 }, versions.Select(version => version.Version).Order());
        Assert.Equal(3, fixture.Room.CurrentVersion);
        var head = fixture.Room.GetVersion(3).RequireValue();
        Assert.NotEqual(fixture.Room.GetVersion(1).RequireValue().Chunks[fixture.Pair.A].ContentHash, head.Chunks[fixture.Pair.A].ContentHash);
        Assert.NotEqual(fixture.Room.GetVersion(1).RequireValue().Chunks[fixture.Pair.B].ContentHash, head.Chunks[fixture.Pair.B].ContentHash);
        Assert.True(fixture.Room.CreateSpliceReport(3).IsSuccess);
    }

    [Fact]
    public void WeldAcrossChunks_RequiresCurrentManifestAndRegistersStableId()
    {
        using var fixture = TestRoom.Create();
        var stale = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        var rejected = stale.WeldAcrossChunks(
            fixture.Pair,
            0,
            fixture.Sample.WeldFirstVertexId,
            fixture.Sample.WeldSecondVertexId);
        Assert.Equal(MeshErrorCode.E_BOUNDARY_STALE, rejected.Error!.Code);
        Assert.Equal(fixture.Pair.Id, rejected.Error.ChunkPair);
        stale.Cancel();

        var weld = fixture.Room.TryBeginEdit("a", new[] { fixture.Pair.A, fixture.Pair.B }).RequireValue();
        var v1 = fixture.Room.GetVersion(1).RequireValue();
        Assert.True(weld.WeldAcrossChunks(
            fixture.Pair,
            v1.Boundaries[fixture.Pair].ManifestVersion,
            fixture.Sample.WeldFirstVertexId,
            fixture.Sample.WeldSecondVertexId).IsSuccess);
        var v2 = weld.Submit().RequireValue();
        var manifest = v2.Boundaries[fixture.Pair];
        Assert.Single(manifest.StableVertexIds);
        Assert.Equal(v2.Version, manifest.ManifestVersion);
        Assert.Equal(28, v2.VertexCount);
    }
}
