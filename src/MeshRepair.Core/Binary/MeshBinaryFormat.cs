using System.Buffers.Binary;
using System.Security.Cryptography;
using MeshRepair.Core.Errors;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;

namespace MeshRepair.Core.Binary;

public sealed record MeshHeader(
    MeshCoordinateType CoordinateType,
    MeshIndexWidth IndexWidth,
    uint VertexCount,
    uint FaceCount,
    ulong Version)
{
    public const int Size = 24;
    public const uint Magic = 0x314D524D; // "MRM1" in little endian.
}

public sealed record MeshBinaryLoadResult(
    MeshHeader Header,
    MeshData Mesh,
    string ContentHash,
    string FileHash,
    long ByteLength);

public sealed record MeshBinaryWriteResult(
    string Path,
    string ContentHash,
    string FileHash,
    long ByteLength,
    ulong Version);

/// <summary>
/// Fixed little-endian format:
/// 0 magic "MRM1", 4 coordinate type, 5 index width, 6 reserved flags,
/// 8 vertex count, 12 face count, 16 mesh version, 24 payload.
/// Coordinates are XYZ tuples encoded in the declared width. Faces are
/// triangle index triples encoded in the declared index width.
/// </summary>
public static class MeshBinaryFormat
{
    public static MeshResult<MeshBinaryLoadResult> Load(string path)
    {
        try
        {
            var bytes = File.ReadAllBytes(path);
            return Read(bytes);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return MeshResult<MeshBinaryLoadResult>.Fail(
                MeshErrorCode.E_IO,
                $"failed to read mesh: {exception.Message}",
                field: "File");
        }
    }

    public static MeshResult<MeshBinaryLoadResult> Read(ReadOnlySpan<byte> bytes)
    {
        if (bytes.Length < 4)
        {
            return Invalid("header truncated before magic", "Magic", 0);
        }

        var magic = BinaryPrimitives.ReadUInt32LittleEndian(bytes[..4]);
        if (magic != MeshHeader.Magic)
        {
            return Invalid("mesh magic must be MRM1", "Magic", 0);
        }

        if (bytes.Length < 6)
        {
            return Invalid("header truncated before coordinate type", "CoordinateType", 4);
        }

        var coordinateType = (MeshCoordinateType)bytes[4];
        if (coordinateType is not MeshCoordinateType.Float32 and not MeshCoordinateType.Float64)
        {
            return Invalid($"unsupported coordinate type {(byte)coordinateType}", "CoordinateType", 4);
        }

        if (bytes.Length < 8)
        {
            return Invalid("header truncated before index width", "IndexWidth", 5);
        }

        var indexWidth = (MeshIndexWidth)bytes[5];
        if (indexWidth is not MeshIndexWidth.UInt16 and not MeshIndexWidth.UInt32)
        {
            return Invalid($"unsupported index width {(byte)indexWidth}", "IndexWidth", 5);
        }

        if (bytes.Length < 16)
        {
            return Invalid("header truncated before mesh counts", "VertexCount", 8);
        }

        var vertexCount = BinaryPrimitives.ReadUInt32LittleEndian(bytes[8..12]);
        var faceCount = BinaryPrimitives.ReadUInt32LittleEndian(bytes[12..16]);

        if (bytes.Length < MeshHeader.Size)
        {
            return Invalid("mesh version field is missing", "Version", 16);
        }

        var version = BinaryPrimitives.ReadUInt64LittleEndian(bytes[16..24]);
        if (version == 0)
        {
            return Invalid("mesh version must be non-zero", "Version", 16);
        }

        var coordinateByteWidth = coordinateType == MeshCoordinateType.Float32 ? 4 : 8;
        var coordinateBytes = (long)vertexCount * 3L * coordinateByteWidth;
        var faceBytes = (long)faceCount * 3L * (int)indexWidth;
        var expectedLength = MeshHeader.Size + coordinateBytes + faceBytes;
        if (expectedLength > int.MaxValue)
        {
            return Invalid("declared mesh is too large", "VertexCount", 8);
        }

        var payloadLength = bytes.Length - MeshHeader.Size;
        if (payloadLength < coordinateBytes)
        {
            return Invalid(
                $"vertex payload requires {coordinateBytes} bytes for {vertexCount} vertices but only {payloadLength} are present",
                "VertexCount",
                8);
        }

        if (payloadLength < coordinateBytes + faceBytes)
        {
            return Invalid(
                $"face payload requires {faceBytes} bytes for {faceCount} faces but only {payloadLength - coordinateBytes} are present",
                "FaceCount",
                12);
        }

        if (payloadLength != coordinateBytes + faceBytes)
        {
            return Invalid(
                $"data length {payloadLength} does not match header payload {coordinateBytes + faceBytes}",
                "DataLength",
                MeshHeader.Size);
        }

        var vertices = new Point3[vertexCount];
        var offset = MeshHeader.Size;
        for (var vertexIndex = 0; vertexIndex < vertexCount; vertexIndex++)
        {
            double x;
            double y;
            double z;
            if (coordinateType == MeshCoordinateType.Float32)
            {
                x = BinaryPrimitives.ReadSingleLittleEndian(bytes.Slice(offset, 4));
                y = BinaryPrimitives.ReadSingleLittleEndian(bytes.Slice(offset + 4, 4));
                z = BinaryPrimitives.ReadSingleLittleEndian(bytes.Slice(offset + 8, 4));
                offset += 12;
            }
            else
            {
                x = BinaryPrimitives.ReadDoubleLittleEndian(bytes.Slice(offset, 8));
                y = BinaryPrimitives.ReadDoubleLittleEndian(bytes.Slice(offset + 8, 8));
                z = BinaryPrimitives.ReadDoubleLittleEndian(bytes.Slice(offset + 16, 8));
                offset += 24;
            }

            var point = new Point3(x, y, z);
            if (!point.IsFinite)
            {
                return Invalid("vertex coordinate is not finite", $"Vertices[{vertexIndex}]", offset);
            }

            vertices[vertexIndex] = point;
        }

        var faces = new Triangle[faceCount];
        for (var faceIndex = 0; faceIndex < faceCount; faceIndex++)
        {
            var indices = new int[3];
            for (var corner = 0; corner < 3; corner++)
            {
                uint index;
                if (indexWidth == MeshIndexWidth.UInt16)
                {
                    index = BinaryPrimitives.ReadUInt16LittleEndian(bytes.Slice(offset, 2));
                    offset += 2;
                }
                else
                {
                    index = BinaryPrimitives.ReadUInt32LittleEndian(bytes.Slice(offset, 4));
                    offset += 4;
                }

                if (index >= vertexCount)
                {
                    return Invalid(
                        $"face index {index} is outside vertex range 0..{vertexCount - 1}",
                        $"Faces[{faceIndex}].Index[{corner}]",
                        offset - (int)indexWidth);
                }

                indices[corner] = checked((int)index);
            }

            faces[faceIndex] = new Triangle(indices[0], indices[1], indices[2]);
        }

        var contentHash = ComputeContentHash(coordinateType, indexWidth, vertices, faces);
        var fileHash = HashUtil.Sha256Hex(bytes);
        var header = new MeshHeader(coordinateType, indexWidth, vertexCount, faceCount, version);
        var mesh = new MeshData(version, coordinateType, indexWidth, vertices, faces, contentHash, fileHash);
        return MeshResult<MeshBinaryLoadResult>.Ok(
            new MeshBinaryLoadResult(header, mesh, contentHash, fileHash, bytes.Length));
    }

    public static MeshResult<MeshBinaryWriteResult> Write(
        string path,
        MeshData mesh,
        ulong? version = null,
        bool atomic = true)
    {
        var writeVersion = version ?? mesh.Version;
        if (writeVersion == 0)
        {
            return MeshResult<MeshBinaryWriteResult>.Fail(
                MeshErrorCode.E_MESH_INVALID,
                "mesh version must be non-zero",
                field: "Version");
        }

        var validation = ValidateIndices(mesh.Vertices, mesh.Faces);
        if (!validation.IsSuccess)
        {
            return MeshResult<MeshBinaryWriteResult>.Fail(validation.Error!);
        }

        if (mesh.IndexWidth == MeshIndexWidth.UInt16 && mesh.Vertices.Count > ushort.MaxValue + 1)
        {
            return MeshResult<MeshBinaryWriteResult>.Fail(
                MeshErrorCode.E_MESH_INVALID,
                "vertex count cannot be represented with a 16-bit index width",
                field: "IndexWidth");
        }

        if (mesh.CoordinateType == MeshCoordinateType.Float32)
        {
            for (var vertexIndex = 0; vertexIndex < mesh.Vertices.Count; vertexIndex++)
            {
                var vertex = mesh.Vertices[vertexIndex];
                if (!float.IsFinite((float)vertex.X)
                    || !float.IsFinite((float)vertex.Y)
                    || !float.IsFinite((float)vertex.Z))
                {
                    return MeshResult<MeshBinaryWriteResult>.Fail(
                        MeshErrorCode.E_MESH_INVALID,
                        "vertex coordinate cannot be represented as Float32",
                        field: $"Vertices[{vertexIndex}]");
                }
            }
        }

        var contentHash = ComputeContentHash(
            mesh.CoordinateType,
            mesh.IndexWidth,
            mesh.Vertices,
            mesh.Faces);
        using var memory = new MemoryStream();
        WriteCore(memory, mesh.CoordinateType, mesh.IndexWidth, mesh.Vertices, mesh.Faces, writeVersion);
        var bytes = memory.ToArray();
        var fileHash = HashUtil.Sha256Hex(bytes);

        try
        {
            var directory = Path.GetDirectoryName(Path.GetFullPath(path));
            if (!string.IsNullOrEmpty(directory))
            {
                Directory.CreateDirectory(directory);
            }

            if (atomic)
            {
                var temporaryPath = path + ".tmp";
                File.WriteAllBytes(temporaryPath, bytes);
                File.Move(temporaryPath, path, overwrite: true);
            }
            else
            {
                File.WriteAllBytes(path, bytes);
            }
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return MeshResult<MeshBinaryWriteResult>.Fail(
                MeshErrorCode.E_IO,
                $"failed to write mesh: {exception.Message}",
                field: "File");
        }

        return MeshResult<MeshBinaryWriteResult>.Ok(
            new MeshBinaryWriteResult(path, contentHash, fileHash, bytes.Length, writeVersion));
    }

    public static string ComputeContentHash(
        MeshCoordinateType coordinateType,
        MeshIndexWidth indexWidth,
        IReadOnlyList<Point3> vertices,
        IReadOnlyList<Triangle> faces)
    {
        using var stream = new MemoryStream();
        using var writer = new BinaryWriter(stream, System.Text.Encoding.UTF8, leaveOpen: true);
        writer.Write((byte)coordinateType);
        writer.Write((byte)indexWidth);
        writer.Write((ushort)0);
        writer.Write((uint)vertices.Count);
        writer.Write((uint)faces.Count);
        WriteVertices(writer, coordinateType, vertices);
        WriteFaces(writer, indexWidth, faces);
        writer.Flush();
        return Convert.ToHexString(SHA256.HashData(stream.ToArray())).ToLowerInvariant();
    }

    public static MeshResult ValidateIndices(
        IReadOnlyList<Point3> vertices,
        IReadOnlyList<Triangle> faces)
    {
        for (var faceIndex = 0; faceIndex < faces.Count; faceIndex++)
        {
            var face = faces[faceIndex];
            for (var corner = 0; corner < 3; corner++)
            {
                var index = face[corner];
                if (index < 0 || index >= vertices.Count)
                {
                    return MeshResult.Fail(
                        MeshErrorCode.E_MESH_INVALID,
                        $"face index {index} is outside vertex range 0..{vertices.Count - 1}",
                        field: $"Faces[{faceIndex}].Index[{corner}]");
                }
            }
        }

        for (var vertexIndex = 0; vertexIndex < vertices.Count; vertexIndex++)
        {
            if (!vertices[vertexIndex].IsFinite)
            {
                return MeshResult.Fail(
                    MeshErrorCode.E_MESH_INVALID,
                    "vertex coordinate is not finite",
                    field: $"Vertices[{vertexIndex}]");
            }
        }

        return MeshResult.Ok();
    }

    private static void WriteCore(
        Stream stream,
        MeshCoordinateType coordinateType,
        MeshIndexWidth indexWidth,
        IReadOnlyList<Point3> vertices,
        IReadOnlyList<Triangle> faces,
        ulong version)
    {
        using var writer = new BinaryWriter(stream, System.Text.Encoding.UTF8, leaveOpen: true);
        writer.Write(MeshHeader.Magic);
        writer.Write((byte)coordinateType);
        writer.Write((byte)indexWidth);
        writer.Write((ushort)0);
        writer.Write((uint)vertices.Count);
        writer.Write((uint)faces.Count);
        writer.Write(version);
        WriteVertices(writer, coordinateType, vertices);
        WriteFaces(writer, indexWidth, faces);
        writer.Flush();
    }

    private static void WriteVertices(
        BinaryWriter writer,
        MeshCoordinateType coordinateType,
        IReadOnlyList<Point3> vertices)
    {
        foreach (var vertex in vertices)
        {
            if (coordinateType == MeshCoordinateType.Float32)
            {
                writer.Write((float)vertex.X);
                writer.Write((float)vertex.Y);
                writer.Write((float)vertex.Z);
            }
            else
            {
                writer.Write(vertex.X);
                writer.Write(vertex.Y);
                writer.Write(vertex.Z);
            }
        }
    }

    private static void WriteFaces(
        BinaryWriter writer,
        MeshIndexWidth indexWidth,
        IReadOnlyList<Triangle> faces)
    {
        foreach (var face in faces)
        {
            for (var corner = 0; corner < 3; corner++)
            {
                if (indexWidth == MeshIndexWidth.UInt16)
                {
                    writer.Write(checked((ushort)face[corner]));
                }
                else
                {
                    writer.Write(checked((uint)face[corner]));
                }
            }
        }
    }

    private static MeshResult<MeshBinaryLoadResult> Invalid(string message, string field, long offset)
        => MeshResult<MeshBinaryLoadResult>.Fail(
            MeshErrorCode.E_MESH_INVALID,
            $"{message} (offset {offset})",
            field: field);
}
