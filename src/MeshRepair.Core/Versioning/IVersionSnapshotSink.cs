using MeshRepair.Core.Errors;

namespace MeshRepair.Core.Versioning;

public interface IVersionSnapshotSink
{
    MeshResult Save(MeshVersion version);
}
