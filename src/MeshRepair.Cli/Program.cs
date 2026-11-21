using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using MeshRepair.Core.Binary;
using MeshRepair.Core.Chunks;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Model;
using MeshRepair.Core.Recovery;
using MeshRepair.Core.Sample;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Cli;

public static class Program
{
    public static int Main(string[] args)
    {
        try
        {
            if (args.Length == 0)
            {
                PrintUsage();
                return 2;
            }

            return args[0] switch
            {
                "sample" => Sample(args),
                "verify" => Verify(args),
                "recover" => Recover(args),
                _ => Unknown(args[0])
            };
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"fatal: {exception}");
            return 1;
        }
    }

    private static int Sample(string[] args)
    {
        var output = RequiredOption(args, "--out");
        var seed = int.Parse(Option(args, "--seed") ?? "1337");
        Directory.CreateDirectory(output);
        var sample = MeshSampleGenerator.Generate(seed);
        var meshPath = Path.Combine(output, "scan.mesh");
        var written = Require(MeshBinaryFormat.Write(meshPath, sample.Mesh, atomic: true));
        var loaded = Require(MeshBinaryFormat.Load(meshPath));
        var layout = ChunkLayout.FromMesh(loaded.Mesh, divisionsX: 2);
        var room = Require(MeshRepairRoom.Create(loaded.Mesh, layout, "baseline-loader"));
        var initial = Require(room.GetVersion(1));
        var trace = new TraceWriter();
        trace.Publish(initial);
        var metadata = new
        {
            seed,
            meshFile = "scan.mesh",
            meshVersion = loaded.Mesh.Version,
            coordinateType = loaded.Mesh.CoordinateType.ToString(),
            indexWidth = loaded.Mesh.IndexWidth.ToString(),
            vertexCount = loaded.Mesh.Vertices.Count,
            faceCount = loaded.Mesh.Faces.Count,
            contentHash = loaded.ContentHash,
            fileHash = written.FileHash,
            weldFirstVertexId = sample.WeldFirstVertexId,
            weldSecondVertexId = sample.WeldSecondVertexId,
            weldPosition = new[] { sample.WeldPosition.X, sample.WeldPosition.Y, sample.WeldPosition.Z },
            holeVertices = new
            {
                bottomLeft = new[] { sample.HoleVertices.BottomLeft.X, sample.HoleVertices.BottomLeft.Y, sample.HoleVertices.BottomLeft.Z },
                bottomRight = new[] { sample.HoleVertices.BottomRight.X, sample.HoleVertices.BottomRight.Y, sample.HoleVertices.BottomRight.Z },
                topRight = new[] { sample.HoleVertices.TopRight.X, sample.HoleVertices.TopRight.Y, sample.HoleVertices.TopRight.Z },
                topLeft = new[] { sample.HoleVertices.TopLeft.X, sample.HoleVertices.TopLeft.Y, sample.HoleVertices.TopLeft.Z }
            },
            fragmentVertices = new[] { sample.FragmentVertexA, sample.FragmentVertexB, sample.FragmentVertexC },
            nonManifoldEdge = sample.NonManifoldEdge,
            degenerateFaceIndex = sample.DegenerateFaceIndex,
            boundaries = initial.Boundaries.Values
                .OrderBy(manifest => manifest.Pair)
                .Select(manifest => new
                {
                    pair = manifest.Pair.Id,
                    version = manifest.ManifestVersion,
                    hash = manifest.ContentHash,
                    vertices = manifest.SharedVertexIds.Count,
                    edges = manifest.Edges.Count
                })
                .ToArray()
        };
        File.WriteAllText(
            Path.Combine(output, "sample.json"),
            JsonSerializer.Serialize(metadata, SnapshotJson.Options),
            new UTF8Encoding(false));
        File.WriteAllText(
            Path.Combine(output, "baseline-trace.jsonl"),
            trace.ToJsonLines(),
            new UTF8Encoding(false));
        Console.WriteLine($"sample={meshPath}");
        Console.WriteLine($"contentHash={loaded.ContentHash}");
        Console.WriteLine($"fileHash={written.FileHash}");
        return 0;
    }

    private static int Verify(string[] args)
    {
        var output = RequiredOption(args, "--out");
        var seed = int.Parse(Option(args, "--seed") ?? "1337");
        var first = VerificationWorkflow.Run(output, seed);
        var repeatDirectory = Path.Combine(
            Path.GetTempPath(),
            $"mesh-repair-determinism-{seed}-{Environment.ProcessId}-{Guid.NewGuid():N}");
        if (Directory.Exists(repeatDirectory))
        {
            Directory.Delete(repeatDirectory, recursive: true);
        }

        try
        {
            var second = VerificationWorkflow.Run(repeatDirectory, seed);
            var traceIdentical = first.TraceText == second.TraceText;
            var versionsIdentical = first.VersionsText == second.VersionsText;
            var snapshotFilesIdentical = CompareSnapshotDirectories(
                Path.Combine(output, "snapshots"),
                Path.Combine(repeatDirectory, "snapshots"));
            var determinism = new
            {
                traceByteIdentical = traceIdentical,
                versionsByteIdentical = versionsIdentical,
                snapshotsByteIdentical = snapshotFilesIdentical,
                traceSha256 = HashUtil.Sha256Hex(first.TraceText),
                versionsSha256 = HashUtil.Sha256Hex(first.VersionsText)
            };
            File.WriteAllText(
                Path.Combine(output, "determinism.json"),
                JsonSerializer.Serialize(determinism, SnapshotJson.Options),
                new UTF8Encoding(false));
            if (!traceIdentical || !versionsIdentical || !snapshotFilesIdentical)
            {
                Console.Error.WriteLine("determinism check failed");
                return 3;
            }
        }
        finally
        {
            if (Directory.Exists(repeatDirectory))
            {
                Directory.Delete(repeatDirectory, recursive: true);
            }
        }

        Console.WriteLine($"headVersion={first.Summary.HeadVersion}");
        Console.WriteLine($"contentHash={first.Summary.HeadContentHash}");
        Console.WriteLine($"trace={Path.Combine(output, "trace.jsonl")}");
        Console.WriteLine($"versions={Path.Combine(output, "versions.json")}");
        return 0;
    }

    private static int Recover(string[] args)
    {
        var directory = RequiredOption(args, "--snapshots");
        var repository = SnapshotRepository.LoadFromDirectory(directory);
        if (!repository.IsSuccess)
        {
            Console.Error.WriteLine(repository.Error);
            return 1;
        }

        var recovered = Require(repository.RequireValue().LoadLatest());
        var room = Require(MeshRepairRoom.Restore(
            recovered.Versions,
            recovered.HeadVersion,
            recovered.Layout,
            recovered.Baseline));
        var splice = Require(room.CreateSpliceReport(recovered.HeadVersion));
        var value = new
        {
            recoveredHead = recovered.HeadVersion,
            skippedSnapshots = recovered.SkippedSnapshots,
            recomputedBlocks = recovered.RecomputedBlocks.Select(chunk => chunk.ToString()).ToArray(),
            verifiedBlockCount = recovered.VerifiedBlocks.Count,
            contentHash = splice.VersionContentHash,
            vertexCount = splice.CurrentVertexCount,
            faceCount = splice.CurrentFaceCount
        };
        Console.WriteLine(JsonSerializer.Serialize(value, SnapshotJson.Options));
        return 0;
    }

    private static bool CompareSnapshotDirectories(string first, string second)
    {
        var firstFiles = Directory.GetFiles(first, "snapshot-*.json").Order().ToArray();
        var secondFiles = Directory.GetFiles(second, "snapshot-*.json").Order().ToArray();
        if (firstFiles.Length != secondFiles.Length)
        {
            return false;
        }

        for (var index = 0; index < firstFiles.Length; index++)
        {
            if (Path.GetFileName(firstFiles[index]) != Path.GetFileName(secondFiles[index]))
            {
                return false;
            }

            if (HashUtil.Sha256File(firstFiles[index]) != HashUtil.Sha256File(secondFiles[index]))
            {
                return false;
            }
        }

        return true;
    }

    private static string RequiredOption(string[] args, string name)
        => Option(args, name) ?? throw new ArgumentException($"missing required option {name}");

    private static string? Option(string[] args, string name)
    {
        var index = Array.IndexOf(args, name);
        return index >= 0 && index + 1 < args.Length ? args[index + 1] : null;
    }

    private static int Unknown(string command)
    {
        Console.Error.WriteLine($"unknown command: {command}");
        PrintUsage();
        return 2;
    }

    private static T Require<T>(MeshRepair.Core.Errors.MeshResult<T> result)
        => result.IsSuccess
            ? result.RequireValue()
            : throw new InvalidOperationException(result.Error!.ToString());

    private static void PrintUsage()
    {
        Console.WriteLine("MeshRepair CLI");
        Console.WriteLine("  sample  --out <dir> [--seed 1337]");
        Console.WriteLine("  verify  --out <dir> [--seed 1337]");
        Console.WriteLine("  recover --snapshots <dir>");
    }
}
