using MeshRepair.Core.Errors;

namespace MeshRepair.Core.Versioning;

public sealed class MeshSnapshotHandle : IDisposable
{
    private readonly MeshVersion _version;
    private int _released;

    internal MeshSnapshotHandle(MeshVersion version)
    {
        _version = version;
    }

    public long Version => _version.Version;

    public MeshResult<MeshVersion> Read()
    {
        if (Volatile.Read(ref _released) != 0)
        {
            return MeshResult<MeshVersion>.Fail(
                MeshErrorCode.E_STALE_HANDLE,
                "snapshot handle has already been released",
                snapshot: _version.Version.ToString(System.Globalization.CultureInfo.InvariantCulture));
        }

        return MeshResult<MeshVersion>.Ok(_version);
    }

    public void Release()
    {
        Interlocked.Exchange(ref _released, 1);
    }

    public void Dispose() => Release();
}
