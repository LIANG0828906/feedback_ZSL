using System.Drawing.Drawing2D;
using TacticalField.Core;

namespace TacticalField.WinForms;

internal sealed class BattlefieldPanel : Panel
{
    private BattlefieldView? _view;
    private FieldKey? _fieldKey;
    private HashSet<int> _path = new();
    private int _selectedCell = -1;

    public BattlefieldPanel()
    {
        DoubleBuffered = true;
        ResizeRedraw = true;
        BackColor = Color.FromArgb(24, 27, 35);
    }

    public int SelectedCell => _selectedCell;

    public void SetState(BattlefieldView view, FieldKey key, IReadOnlyList<int> path, int selectedCell)
    {
        _view = view;
        _fieldKey = key;
        _path = path.ToHashSet();
        _selectedCell = selectedCell;
        Invalidate();
    }

    protected override void OnMouseClick(MouseEventArgs e)
    {
        base.OnMouseClick(e);
        if (_view is null)
        {
            return;
        }

        var geometry = Geometry();
        var x = (e.X - geometry.OffsetX) / geometry.CellSize;
        var y = (e.Y - geometry.OffsetY) / geometry.CellSize;
        if (x >= 0 && x < _view.Grid.Width && y >= 0 && y < _view.Grid.Height)
        {
            _selectedCell = y * _view.Grid.Width + x;
            Invalidate();
        }
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        e.Graphics.Clear(BackColor);
        if (_view is null || _fieldKey is null || !_view.Fields.TryGetValue(_fieldKey, out var field))
        {
            return;
        }

        var geometry = Geometry();
        var minCost = long.MaxValue;
        var maxCost = long.MinValue;
        for (var index = 0; index < field.CellCount; index++)
        {
            var cost = field.GetCost(index);
            if (cost >= 0)
            {
                minCost = Math.Min(minCost, cost);
                maxCost = Math.Max(maxCost, cost);
            }
        }

        if (minCost == long.MaxValue)
        {
            minCost = 0;
            maxCost = 1;
        }

        using var borderPen = new Pen(Color.FromArgb(75, 82, 100), 1f);
        using var selectedPen = new Pen(Color.FromArgb(255, 220, 80), 3f);
        using var pathPen = new Pen(Color.FromArgb(90, 210, 255), 3f);
        using var normalFont = new Font("Consolas", Math.Max(7f, geometry.CellSize * 0.20f), FontStyle.Regular);
        using var smallFont = new Font("Consolas", Math.Max(6f, geometry.CellSize * 0.16f), FontStyle.Regular);
        using var markerFont = new Font("Segoe UI", Math.Max(7f, geometry.CellSize * 0.24f), FontStyle.Bold);

        var unit = _view.Units[_fieldKey.UnitId];
        for (var index = 0; index < field.CellCount; index++)
        {
            var x = index % _view.Grid.Width;
            var y = index / _view.Grid.Width;
            var rectangle = new Rectangle(
                geometry.OffsetX + x * geometry.CellSize,
                geometry.OffsetY + y * geometry.CellSize,
                geometry.CellSize - 1,
                geometry.CellSize - 1);

            var obstacle = _view.Grid.IsObstacle(index);
            var hazard = _view.Grid.IsHazard(index);
            var cost = field.GetCost(index);
            using var fill = new SolidBrush(CellColor(cost, hazard, obstacle, minCost, maxCost));
            e.Graphics.FillRectangle(fill, rectangle);

            if (_path.Contains(index))
            {
                using var pathBrush = new SolidBrush(Color.FromArgb(72, 80, 210, 255));
                e.Graphics.FillRectangle(pathBrush, rectangle);
                e.Graphics.DrawRectangle(pathPen, rectangle);
            }
            else
            {
                e.Graphics.DrawRectangle(borderPen, rectangle);
            }

            if (index == _selectedCell)
            {
                e.Graphics.DrawRectangle(selectedPen, rectangle);
            }

            var mask = _view.Grid.PassMaskAt(index);
            var label = $"M{mask} c{_view.Grid.BaseCostAt(index)}";
            e.Graphics.DrawString(label, smallFont, Brushes.White, rectangle.Left + 3, rectangle.Top + 2);
            if (cost >= 0)
            {
                e.Graphics.DrawString(cost.ToString(System.Globalization.CultureInfo.InvariantCulture), normalFont, Brushes.White, rectangle.Left + 3, rectangle.Bottom - geometry.CellSize * 0.40f);
            }
            else if (!obstacle)
            {
                e.Graphics.DrawString("--", normalFont, Brushes.Gainsboro, rectangle.Left + 3, rectangle.Bottom - geometry.CellSize * 0.40f);
            }

            if (obstacle)
            {
                e.Graphics.DrawString("X", markerFont, Brushes.OrangeRed, rectangle.Right - geometry.CellSize * 0.32f, rectangle.Top + 2);
            }
            else if (hazard)
            {
                e.Graphics.DrawString("H", markerFont, Brushes.Gold, rectangle.Right - geometry.CellSize * 0.32f, rectangle.Top + 2);
            }

            if (index == unit.StartCell)
            {
                e.Graphics.DrawString("S", markerFont, Brushes.Lime, rectangle.Left + 3, rectangle.Bottom - geometry.CellSize * 0.72f);
            }

            if (index == _fieldKey.Target)
            {
                e.Graphics.DrawString("T", markerFont, Brushes.DeepSkyBlue, rectangle.Right - geometry.CellSize * 0.32f, rectangle.Bottom - geometry.CellSize * 0.72f);
            }
        }

        var title = $"{_fieldKey.UnitId} -> {_fieldKey.Target}  v{field.FieldVersion}  {field.ContentHash[..12]}";
        e.Graphics.DrawString(title, markerFont, Brushes.White, geometry.OffsetX, geometry.OffsetY - 26);
    }

    private GeometryResult Geometry()
    {
        if (_view is null || _view.Grid.Width <= 0 || _view.Grid.Height <= 0)
        {
            return new GeometryResult(28, 28, 28, 28);
        }

        const int margin = 30;
        var cell = Math.Min(
            Math.Max(18, (Width - margin * 2) / _view.Grid.Width),
            Math.Max(18, (Height - margin * 2) / _view.Grid.Height));
        var totalWidth = cell * _view.Grid.Width;
        var totalHeight = cell * _view.Grid.Height;
        return new GeometryResult(
            Math.Max(margin, (Width - totalWidth) / 2),
            Math.Max(margin, (Height - totalHeight) / 2),
            cell,
            totalHeight);
    }

    private static Color CellColor(long cost, bool hazard, bool obstacle, long minCost, long maxCost)
    {
        if (obstacle)
        {
            return Color.FromArgb(68, 68, 72);
        }

        if (cost < 0)
        {
            return Color.FromArgb(18, 22, 36);
        }

        var ratio = maxCost == minCost ? 0 : (double)(cost - minCost) / (maxCost - minCost);
        ratio = Math.Clamp(ratio, 0, 1);
        var low = Color.FromArgb(35, 110, 180);
        var middle = Color.FromArgb(62, 150, 92);
        var high = Color.FromArgb(190, 78, 45);
        var color = ratio < 0.5
            ? Blend(low, middle, ratio * 2)
            : Blend(middle, high, (ratio - 0.5) * 2);
        return hazard ? Blend(color, Color.FromArgb(150, 30, 35), 0.45) : color;
    }

    private static Color Blend(Color first, Color second, double ratio)
    {
        ratio = Math.Clamp(ratio, 0, 1);
        return Color.FromArgb(
            (int)(first.A + (second.A - first.A) * ratio),
            (int)(first.R + (second.R - first.R) * ratio),
            (int)(first.G + (second.G - first.G) * ratio),
            (int)(first.B + (second.B - first.B) * ratio));
    }

    private readonly record struct GeometryResult(int OffsetX, int OffsetY, int CellSize, int TotalHeight);
}

