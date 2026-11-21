using System.Text.Json;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Recovery;
using MeshRepair.Core.Versioning;
using OpenTK.Windowing.Common;
using OpenTK.Windowing.Desktop;

namespace MeshRepair.Preview;

public static class Program
{
    public static int Main(string[] args)
    {
        try
        {
            var snapshots = RequiredOption(args, "--snapshots");
            var repository = SnapshotRepository.LoadFromDirectory(snapshots);
            if (!repository.IsSuccess)
            {
                Console.Error.WriteLine(repository.Error);
                return 1;
            }

            var recovered = repository.RequireValue().LoadLatest();
            if (!recovered.IsSuccess)
            {
                Console.Error.WriteLine(recovered.Error);
                return 1;
            }

            var recovery = recovered.RequireValue();
            var roomResult = MeshRepairRoom.Restore(
                recovery.Versions,
                recovery.HeadVersion,
                recovery.Layout,
                recovery.Baseline);
            if (!roomResult.IsSuccess)
            {
                Console.Error.WriteLine(roomResult.Error);
                return 1;
            }

            var room = roomResult.RequireValue();
            if (args.Contains("--headless", StringComparer.Ordinal))
            {
                var output = Option(args, "--out") ?? "preview.ppm";
                using var handle = room.AcquireSnapshot().RequireValue();
                var read = handle.Read();
                var geometry = PreviewGeometryBuilder.Build(read.RequireValue());
                SoftwarePreviewRenderer.RenderPpm(geometry, width: 960, height: 560, output);
                var metadata = new
                {
                    version = geometry.Version,
                    contentHash = geometry.ContentHash,
                    chunks = geometry.Chunks.Select(chunk => chunk.ToString()).ToArray(),
                    triangleVertices = geometry.Triangles.Count,
                    boundarySegments = geometry.BoundaryLines.Count,
                    mode = "headless-software",
                    output = Path.GetFileName(output)
                };
                var metadataPath = Path.ChangeExtension(output, ".json");
                File.WriteAllText(metadataPath, JsonSerializer.Serialize(metadata, SnapshotJson.Options));
                Console.WriteLine($"preview={output}");
                return 0;
            }

            var gameSettings = GameWindowSettings.Default;
            var nativeSettings = new NativeWindowSettings
            {
                ClientSize = new OpenTK.Mathematics.Vector2i(1024, 720),
                Title = "Mesh Repair - Published Snapshot Preview"
            };
            using var window = new PreviewWindow(room, gameSettings, nativeSettings);
            window.Run();
            return 0;
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"fatal: {exception}");
            return 1;
        }
    }

    private static string RequiredOption(string[] args, string name)
        => Option(args, name) ?? throw new ArgumentException($"missing required option {name}");

    private static string? Option(string[] args, string name)
    {
        var index = Array.IndexOf(args, name);
        return index >= 0 && index + 1 < args.Length ? args[index + 1] : null;
    }
}
