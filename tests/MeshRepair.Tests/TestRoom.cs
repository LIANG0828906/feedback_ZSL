using MeshRepair.Core.Chunks;
using MeshRepair.Core.Model;
using MeshRepair.Core.Recovery;
using MeshRepair.Core.Sample;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Tests;

internal sealed class TestRoom : IDisposable
{
    private TestRoom(
        MeshRepairRoom room,
        MeshSample sample,
        SnapshotRepository? repository,
        string directory)
    {
        Room = room;
        Sample = sample;
        Repository = repository;
        Directory = directory;
        Pair = ChunkPair.Create(new ChunkId(0, 0, 0), new ChunkId(1, 0, 0));
    }

    public MeshRepairRoom Room { get; }

    public MeshSample Sample { get; }

    public SnapshotRepository? Repository { get; }

    public string Directory { get; }

    public ChunkPair Pair { get; }

    public static TestRoom Create(bool withSnapshots = false)
    {
        var sample = MeshSampleGenerator.Generate(1337);
        var layout = ChunkLayout.FromMesh(sample.Mesh, divisionsX: 2);
        var baseline = BaselineInfo.FromMesh(sample.Mesh);
        var directory = Path.Combine(Path.GetTempPath(), $"mesh-repair-test-{Guid.NewGuid():N}");
        System.IO.Directory.CreateDirectory(directory);
        SnapshotRepository? repository = null;
        if (withSnapshots)
        {
            repository = new SnapshotRepository(Path.Combine(directory, "snapshots"), layout, baseline);
        }

        var created = MeshRepairRoom.Create(sample.Mesh, layout, "test-loader", repository);
        if (!created.IsSuccess)
        {
            throw new InvalidOperationException(created.Error!.ToString());
        }

        return new TestRoom(created.RequireValue(), sample, repository, directory);
    }

    public void Dispose()
    {
        if (System.IO.Directory.Exists(Directory))
        {
            System.IO.Directory.Delete(Directory, recursive: true);
        }
    }
}
