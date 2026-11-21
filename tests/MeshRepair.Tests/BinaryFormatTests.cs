using System.Buffers.Binary;
using MeshRepair.Core.Binary;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Sample;

namespace MeshRepair.Tests;

public sealed class BinaryFormatTests
{
    [Fact]
    public void RoundTrip_PreservesHeaderCountsTypesAndHashes()
    {
        var sample = MeshSampleGenerator.Generate(1337);
        var path = Path.Combine(Path.GetTempPath(), $"mesh-{Guid.NewGuid():N}.mesh");
        try
        {
            var write = MeshBinaryFormat.Write(path, sample.Mesh);
            Assert.True(write.IsSuccess, write.Error?.ToString());
            var loaded = MeshBinaryFormat.Load(path);
            Assert.True(loaded.IsSuccess, loaded.Error?.ToString());
            var value = loaded.RequireValue();
            Assert.Equal((uint)sample.Mesh.Vertices.Count, value.Header.VertexCount);
            Assert.Equal((uint)sample.Mesh.Faces.Count, value.Header.FaceCount);
            Assert.Equal(sample.Mesh.CoordinateType, value.Header.CoordinateType);
            Assert.Equal(sample.Mesh.IndexWidth, value.Header.IndexWidth);
            Assert.Equal(sample.Mesh.ContentHash, value.ContentHash);
            Assert.Equal(write.RequireValue().FileHash, value.FileHash);
            Assert.Equal(MeshHeader.Size, 24);
        }
        finally
        {
            File.Delete(path);
        }
    }

    [Fact]
    public void OutOfRangeIndex_ReturnsFirstFaceField()
    {
        var sample = MeshSampleGenerator.Generate(1337);
        var path = Path.Combine(Path.GetTempPath(), $"mesh-{Guid.NewGuid():N}.mesh");
        try
        {
            Assert.True(MeshBinaryFormat.Write(path, sample.Mesh).IsSuccess);
            var bytes = File.ReadAllBytes(path);
            var faceOffset = MeshHeader.Size + (sample.Mesh.Vertices.Count * 3 * 4);
            BinaryPrimitives.WriteUInt16LittleEndian(bytes.AsSpan(faceOffset, 2), ushort.MaxValue);
            File.WriteAllBytes(path, bytes);
            var loaded = MeshBinaryFormat.Load(path);
            Assert.False(loaded.IsSuccess);
            Assert.Equal(MeshErrorCode.E_MESH_INVALID, loaded.Error!.Code);
            Assert.Equal("Faces[0].Index[0]", loaded.Error.Field);
        }
        finally
        {
            File.Delete(path);
        }
    }

    [Fact]
    public void TruncatedFacePayload_ReturnsFaceCountField()
    {
        var sample = MeshSampleGenerator.Generate(1337);
        var path = Path.Combine(Path.GetTempPath(), $"mesh-{Guid.NewGuid():N}.mesh");
        try
        {
            Assert.True(MeshBinaryFormat.Write(path, sample.Mesh).IsSuccess);
            var bytes = File.ReadAllBytes(path);
            File.WriteAllBytes(path, bytes[..^4]);
            var loaded = MeshBinaryFormat.Load(path);
            Assert.False(loaded.IsSuccess);
            Assert.Equal(MeshErrorCode.E_MESH_INVALID, loaded.Error!.Code);
            Assert.Equal("FaceCount", loaded.Error.Field);
        }
        finally
        {
            File.Delete(path);
        }
    }

    [Fact]
    public void MissingVersion_ReturnsVersionField()
    {
        var sample = MeshSampleGenerator.Generate(1337);
        var path = Path.Combine(Path.GetTempPath(), $"mesh-{Guid.NewGuid():N}.mesh");
        try
        {
            Assert.True(MeshBinaryFormat.Write(path, sample.Mesh).IsSuccess);
            var bytes = File.ReadAllBytes(path);
            File.WriteAllBytes(path, bytes[..16]);
            var loaded = MeshBinaryFormat.Load(path);
            Assert.False(loaded.IsSuccess);
            Assert.Equal(MeshErrorCode.E_MESH_INVALID, loaded.Error!.Code);
            Assert.Equal("Version", loaded.Error.Field);
        }
        finally
        {
            File.Delete(path);
        }
    }
}
