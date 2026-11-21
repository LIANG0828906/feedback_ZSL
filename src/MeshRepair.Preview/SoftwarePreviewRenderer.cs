using System.Globalization;
using System.Text;
using MeshRepair.Core.Geometry;

namespace MeshRepair.Preview;

public static class SoftwarePreviewRenderer
{
    public static void RenderPpm(PreviewGeometry geometry, int width, int height, string path)
    {
        if (width <= 0 || height <= 0)
        {
            throw new ArgumentOutOfRangeException(nameof(width));
        }

        var pixels = new uint[width * height];
        Array.Fill(pixels, 0x111827u);
        var bounds = geometry.Bounds;
        if (bounds.IsEmpty)
        {
            bounds = new Bounds3(Point3.Origin, new Point3(1, 1, 0));
        }

        var spanX = Math.Max(1e-6, bounds.Max.X - bounds.Min.X);
        var spanY = Math.Max(1e-6, bounds.Max.Y - bounds.Min.Y);
        var scale = Math.Min((width - 40) / spanX, (height - 40) / spanY);
        var offsetX = (width - (spanX * scale)) / 2.0;
        var offsetY = (height - (spanY * scale)) / 2.0;

        (double X, double Y) Project(Point3 point)
            => (
                offsetX + ((point.X - bounds.Min.X) * scale),
                height - (offsetY + ((point.Y - bounds.Min.Y) * scale)));

        for (var index = 0; index + 2 < geometry.Triangles.Count; index += 3)
        {
            var a = Project(geometry.Triangles[index].Position);
            var b = Project(geometry.Triangles[index + 1].Position);
            var c = Project(geometry.Triangles[index + 2].Position);
            FillTriangle(pixels, width, height, a, b, c, geometry.Triangles[index].PackedColor);
        }

        foreach (var line in geometry.BoundaryLines)
        {
            DrawLine(pixels, width, height, Project(line.A), Project(line.B), line.PackedColor);
        }

        WritePpm(path, width, height, pixels);
    }

    private static void FillTriangle(
        uint[] pixels,
        int width,
        int height,
        (double X, double Y) a,
        (double X, double Y) b,
        (double X, double Y) c,
        uint color)
    {
        var minX = Math.Max(0, (int)Math.Floor(Math.Min(a.X, Math.Min(b.X, c.X))));
        var maxX = Math.Min(width - 1, (int)Math.Ceiling(Math.Max(a.X, Math.Max(b.X, c.X))));
        var minY = Math.Max(0, (int)Math.Floor(Math.Min(a.Y, Math.Min(b.Y, c.Y))));
        var maxY = Math.Min(height - 1, (int)Math.Ceiling(Math.Max(a.Y, Math.Max(b.Y, c.Y))));
        var denominator = ((b.Y - c.Y) * (a.X - c.X)) + ((c.X - b.X) * (a.Y - c.Y));
        if (Math.Abs(denominator) < 1e-12)
        {
            return;
        }

        for (var y = minY; y <= maxY; y++)
        {
            for (var x = minX; x <= maxX; x++)
            {
                var px = x + 0.5;
                var py = y + 0.5;
                var w1 = (((b.Y - c.Y) * (px - c.X)) + ((c.X - b.X) * (py - c.Y))) / denominator;
                var w2 = (((c.Y - a.Y) * (px - c.X)) + ((a.X - c.X) * (py - c.Y))) / denominator;
                var w3 = 1.0 - w1 - w2;
                if (w1 >= -1e-9 && w2 >= -1e-9 && w3 >= -1e-9)
                {
                    pixels[(y * width) + x] = Shade(color);
                }
            }
        }
    }

    private static void DrawLine(
        uint[] pixels,
        int width,
        int height,
        (double X, double Y) a,
        (double X, double Y) b,
        uint color)
    {
        var x0 = (int)Math.Round(a.X);
        var y0 = (int)Math.Round(a.Y);
        var x1 = (int)Math.Round(b.X);
        var y1 = (int)Math.Round(b.Y);
        var dx = Math.Abs(x1 - x0);
        var sx = x0 < x1 ? 1 : -1;
        var dy = -Math.Abs(y1 - y0);
        var sy = y0 < y1 ? 1 : -1;
        var error = dx + dy;
        while (true)
        {
            if (x0 >= 0 && x0 < width && y0 >= 0 && y0 < height)
            {
                pixels[(y0 * width) + x0] = color;
            }

            if (x0 == x1 && y0 == y1)
            {
                break;
            }

            var twice = 2 * error;
            if (twice >= dy)
            {
                error += dy;
                x0 += sx;
            }

            if (twice <= dx)
            {
                error += dx;
                y0 += sy;
            }
        }
    }

    private static uint Shade(uint color)
    {
        var r = (color >> 16) & 0xFF;
        var g = (color >> 8) & 0xFF;
        var b = color & 0xFF;
        return (r << 16) | (g << 8) | b;
    }

    private static void WritePpm(string path, int width, int height, IReadOnlyList<uint> pixels)
    {
        var directory = Path.GetDirectoryName(Path.GetFullPath(path));
        if (!string.IsNullOrEmpty(directory))
        {
            Directory.CreateDirectory(directory);
        }

        using var stream = new FileStream(path, FileMode.Create, FileAccess.Write, FileShare.None);
        var header = Encoding.ASCII.GetBytes($"P6\n{width.ToString(CultureInfo.InvariantCulture)} {height.ToString(CultureInfo.InvariantCulture)}\n255\n");
        stream.Write(header);
        var row = new byte[width * 3];
        for (var y = 0; y < height; y++)
        {
            for (var x = 0; x < width; x++)
            {
                var color = pixels[(y * width) + x];
                row[(x * 3) + 0] = (byte)((color >> 16) & 0xFF);
                row[(x * 3) + 1] = (byte)((color >> 8) & 0xFF);
                row[(x * 3) + 2] = (byte)(color & 0xFF);
            }

            stream.Write(row);
        }
    }
}
