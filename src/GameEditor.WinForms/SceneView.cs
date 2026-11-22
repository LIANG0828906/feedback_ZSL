using System.Drawing.Drawing2D;
using System.Numerics;
using GameEditor.Spatial;

namespace GameEditor.WinForms;

internal sealed class SceneView : Control
{
    private SpatialRuntime? _runtime;
    private IReadOnlyList<RenderGeometry> _renderGeometry = [];
    private HashSet<string> _selection = new(StringComparer.Ordinal);
    private IReadOnlyDictionary<string, Matrix4x4>? _previewWorld;
    private Vector3? _pivot;
    private Point? _dragStart;
    private Point? _dragCurrent;

    public SceneView()
    {
        DoubleBuffered = true;
        BackColor = Color.FromArgb(18, 22, 28);
        TabStop = true;
        Cursor = Cursors.Cross;
    }

    public event EventHandler<IReadOnlyList<string>>? SelectionChanged;
    public event EventHandler<string>? StatusChanged;

    public void SetRuntime(SpatialRuntime runtime)
    {
        _runtime = runtime;
        var render = runtime.GetRenderGeometry();
        if (render.IsSuccess)
        {
            _renderGeometry = render.Value!;
        }

        Invalidate();
    }

    public void SetSelection(IEnumerable<string> selection)
    {
        _selection = new HashSet<string>(selection, StringComparer.Ordinal);
        Invalidate();
    }

    public void SetPreview(
        IReadOnlyDictionary<string, Matrix4x4>? previewWorld,
        Vector3? pivot)
    {
        _previewWorld = previewWorld;
        _pivot = pivot;
        Invalidate();
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        e.Graphics.Clear(BackColor);
        if (_runtime is null)
        {
            DrawCenteredMessage(e.Graphics, "No scene loaded.");
            return;
        }

        var frame = GetFrame(out var scale, out var offsetX, out var offsetY);
        using var framePen = new Pen(Color.FromArgb(55, 70, 85), 1);
        e.Graphics.DrawRectangle(framePen, frame);

        foreach (var geometry in _renderGeometry)
        {
            var bounds = geometry.WorldBounds;
            if (_previewWorld is not null && _previewWorld.TryGetValue(geometry.NodeId, out var preview))
            {
                bounds = geometry.LocalBounds.Transform(preview);
            }

            var projected = _runtime.Camera.ProjectBounds(bounds);
            if (!projected.IsValid)
            {
                continue;
            }

            var screen = ToControl(projected, scale, offsetX, offsetY);
            var selected = _selection.Contains(geometry.NodeId);
            var color = !geometry.IsReady
                ? Color.FromArgb(240, 140, 60)
                : selected
                    ? Color.FromArgb(80, 220, 210)
                    : geometry.Kind switch
                    {
                        GeometryKind.Aabb => Color.FromArgb(125, 150, 175),
                        GeometryKind.Sphere => Color.FromArgb(140, 170, 120),
                        _ => Color.FromArgb(170, 135, 185)
                    };

            using var fill = new SolidBrush(Color.FromArgb(selected ? 62 : 28, color));
            using var pen = new Pen(color, selected ? 2.2f : 1.2f);
            if (geometry.Kind == GeometryKind.Sphere && screen.Width > 2 && screen.Height > 2)
            {
                e.Graphics.FillEllipse(fill, screen);
                e.Graphics.DrawEllipse(pen, screen);
            }
            else
            {
                e.Graphics.FillRectangle(fill, screen);
                e.Graphics.DrawRectangle(pen, screen);
            }

            if (selected)
            {
                using var textBrush = new SolidBrush(Color.FromArgb(225, 245, 245));
                e.Graphics.DrawString(
                    geometry.NodeId,
                    Font,
                    textBrush,
                    screen.Left + 3,
                    MathF.Max(2, screen.Top - Font.Height - 2));
            }
        }

        if (_pivot is { } pivot)
        {
            if (_runtime.Camera.TryProjectPoint(pivot, out var pivotScreen))
            {
                var point = ToControl(pivotScreen, scale, offsetX, offsetY);
                using var pivotPen = new Pen(Color.Gold, 2);
                e.Graphics.DrawLine(pivotPen, point.X - 9, point.Y, point.X + 9, point.Y);
                e.Graphics.DrawLine(pivotPen, point.X, point.Y - 9, point.X, point.Y + 9);
                e.Graphics.DrawEllipse(pivotPen, point.X - 5, point.Y - 5, 10, 10);
            }
        }

        if (_dragStart is { } start && _dragCurrent is { } current)
        {
            var rect = Normalize(start, current);
            using var brush = new SolidBrush(Color.FromArgb(38, 90, 170, 255));
            using var pen = new Pen(Color.FromArgb(220, 120, 190, 255), 1.5f) { DashStyle = DashStyle.Dash };
            e.Graphics.FillRectangle(brush, rect);
            e.Graphics.DrawRectangle(pen, rect);
        }

        using var hud = new SolidBrush(Color.FromArgb(190, 220, 230, 240));
        e.Graphics.DrawString(
            "Click: ray pick   Drag: box select",
            Font,
            hud,
            10,
            Height - Font.Height - 10);
    }

    protected override void OnMouseDown(MouseEventArgs e)
    {
        base.OnMouseDown(e);
        if (e.Button != MouseButtons.Left)
        {
            return;
        }

        Focus();
        _dragStart = e.Location;
        _dragCurrent = e.Location;
        Capture = true;
    }

    protected override void OnMouseMove(MouseEventArgs e)
    {
        base.OnMouseMove(e);
        if (_dragStart is not null)
        {
            _dragCurrent = e.Location;
            Invalidate();
        }
    }

    protected override void OnMouseUp(MouseEventArgs e)
    {
        base.OnMouseUp(e);
        if (_dragStart is null || _runtime is null)
        {
            return;
        }

        var start = _dragStart.Value;
        var end = e.Location;
        _dragStart = null;
        _dragCurrent = null;
        Capture = false;
        var rect = Normalize(start, end);
        if (rect.Width < 4 && rect.Height < 4)
        {
            PickAt(end);
        }
        else
        {
            SelectRectangle(rect);
        }

        Invalidate();
    }

    private void PickAt(Point point)
    {
        if (_runtime is null || !TryToCamera(point, out var screen))
        {
            return;
        }

        var hit = _runtime.Raycast(new RayQueryDefinition
        {
            Id = "ui-ray",
            ScreenX = screen.X,
            ScreenY = screen.Y
        });
        if (!hit.IsSuccess)
        {
            StatusChanged?.Invoke(this, hit.Error!.ToString());
            return;
        }

        var selection = hit.Value is null ? [] : new[] { hit.Value.NodeId };
        _selection = new HashSet<string>(selection, StringComparer.Ordinal);
        SelectionChanged?.Invoke(this, selection);
        StatusChanged?.Invoke(
            this,
            hit.Value is null
                ? "Ray miss."
                : $"Ray hit {hit.Value.NodeId}/{hit.Value.GeometryId}, t={SpatialMath.F(hit.Value.Distance)}");
        Invalidate();
    }

    private void SelectRectangle(Rectangle controlRect)
    {
        if (_runtime is null)
        {
            return;
        }

        var topLeft = ToCamera(new Point(controlRect.Left, controlRect.Top));
        var bottomRight = ToCamera(new Point(controlRect.Right, controlRect.Bottom));
        var width = MathF.Min(_runtime.Camera.Width, bottomRight.X) - MathF.Max(0, topLeft.X);
        var height = MathF.Min(_runtime.Camera.Height, bottomRight.Y) - MathF.Max(0, topLeft.Y);
        var rect = new BoxQueryDefinition
        {
            Id = "ui-box",
            Rect =
            [
                MathF.Max(0, topLeft.X),
                MathF.Max(0, topLeft.Y),
                MathF.Max(0, width),
                MathF.Max(0, height)
            ]
        };
        var result = _runtime.SelectBox(rect);
        if (!result.IsSuccess)
        {
            StatusChanged?.Invoke(this, result.Error!.ToString());
            return;
        }

        _selection = new HashSet<string>(result.Value!, StringComparer.Ordinal);
        SelectionChanged?.Invoke(this, result.Value!);
        StatusChanged?.Invoke(this, $"Box selected {result.Value!.Count} node(s).");
        Invalidate();
    }

    private Rectangle GetFrame(out float scale, out float offsetX, out float offsetY)
    {
        var camera = _runtime!.Camera;
        scale = MathF.Min(Width / (float)camera.Width, Height / (float)camera.Height);
        var width = MathF.Round(camera.Width * scale);
        var height = MathF.Round(camera.Height * scale);
        offsetX = (Width - width) * 0.5f;
        offsetY = (Height - height) * 0.5f;
        return new Rectangle((int)offsetX, (int)offsetY, (int)width, (int)height);
    }

    private RectangleF ToControl(ProjectedBounds bounds, float scale, float offsetX, float offsetY) =>
        new(
            offsetX + bounds.MinX * scale,
            offsetY + bounds.MinY * scale,
            MathF.Max(1, (bounds.MaxX - bounds.MinX) * scale),
            MathF.Max(1, (bounds.MaxY - bounds.MinY) * scale));

    private PointF ToControl(Vector2 cameraPoint, float scale, float offsetX, float offsetY) =>
        new(offsetX + cameraPoint.X * scale, offsetY + cameraPoint.Y * scale);

    private Vector2 ToCamera(Point controlPoint)
    {
        GetFrame(out var scale, out var offsetX, out var offsetY);
        return new Vector2(
            (controlPoint.X - offsetX) / MathF.Max(scale, 0.0001f),
            (controlPoint.Y - offsetY) / MathF.Max(scale, 0.0001f));
    }

    private bool TryToCamera(Point point, out Vector2 cameraPoint)
    {
        cameraPoint = ToCamera(point);
        return _runtime is not null &&
               cameraPoint.X >= 0 &&
               cameraPoint.Y >= 0 &&
               cameraPoint.X <= _runtime.Camera.Width &&
               cameraPoint.Y <= _runtime.Camera.Height;
    }

    private void DrawCenteredMessage(Graphics graphics, string message)
    {
        var size = graphics.MeasureString(message, Font);
        using var brush = new SolidBrush(Color.FromArgb(180, 220, 230, 240));
        graphics.DrawString(
            message,
            Font,
            brush,
            (Width - size.Width) * 0.5f,
            (Height - size.Height) * 0.5f);
    }

    private static Rectangle Normalize(Point a, Point b) =>
        new(
            Math.Min(a.X, b.X),
            Math.Min(a.Y, b.Y),
            Math.Abs(a.X - b.X),
            Math.Abs(a.Y - b.Y));
}
