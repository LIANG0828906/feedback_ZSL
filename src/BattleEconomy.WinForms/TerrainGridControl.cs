using System.Drawing.Drawing2D;
using BattleEconomy.Core;

namespace BattleEconomy.WinForms;

public sealed class TerrainGridControl : Control
{
    private static readonly Color PanelColor = Color.FromArgb(18, 25, 35);
    private static readonly Color GridColor = Color.FromArgb(48, 62, 78);
    private static readonly Color DeadColor = Color.FromArgb(82, 32, 41);
    private static readonly Color DeadStripeColor = Color.FromArgb(221, 74, 86);
    private static readonly Color AuroraColor = Color.FromArgb(76, 201, 176);
    private static readonly Color BastionColor = Color.FromArgb(242, 166, 90);
    private static readonly Color NeutralColor = Color.FromArgb(67, 79, 94);
    private static readonly Color HighlightColor = Color.FromArgb(255, 209, 102);

    private TerrainMap? _map;
    private EconomySnapshot? _snapshot;
    private HashSet<int> _selected = [];

    public TerrainGridControl()
    {
        DoubleBuffered = true;
        ResizeRedraw = true;
        BackColor = PanelColor;
        Cursor = Cursors.Hand;
    }

    public event Action<int>? TileClicked;

    public void SetState(TerrainMap map, EconomySnapshot snapshot, IEnumerable<int> selected)
    {
        _map = map;
        _snapshot = snapshot;
        _selected = selected.ToHashSet();
        Invalidate();
    }

    protected override void OnMouseClick(MouseEventArgs e)
    {
        base.OnMouseClick(e);
        if (_map is null)
        {
            return;
        }

        var geometry = CalculateGeometry(_map);
        for (var row = 0; row < geometry.Rows; row++)
        {
            for (var column = 0; column < geometry.Columns; column++)
            {
                if (!geometry.Cell(row, column).Contains(e.Location))
                {
                    continue;
                }

                var id = row * geometry.Columns + column;
                if (_map.TryGetNode(id, out _))
                {
                    TileClicked?.Invoke(id);
                }

                return;
            }
        }
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        using var background = new SolidBrush(PanelColor);
        e.Graphics.FillRectangle(background, ClientRectangle);
        if (_map is null || _snapshot is null)
        {
            DrawEmpty(e.Graphics);
            return;
        }

        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        var geometry = CalculateGeometry(_map);
        var font = new Font("Segoe UI", Math.Max(6.5f, Math.Min(9f, geometry.CellSize / 4.4f)));
        foreach (var node in _map.Nodes)
        {
            var cell = geometry.Cell(node.Row, node.Column);
            var contribution = _snapshot.Contributions.TryGetValue(node.Id, out var value)
                ? value
                : null;
            var fill = contribution?.SuppliedFaction switch
            {
                Faction.Aurora => Blend(AuroraColor, 0.52f),
                Faction.Bastion => Blend(BastionColor, 0.52f),
                _ => NeutralColor
            };

            if (node.IsHarvestable && contribution?.SuppliedFaction is null)
            {
                fill = Color.FromArgb(39, 50, 63);
            }

            if (contribution?.IsDestroyed == true)
            {
                fill = DeadColor;
            }

            using (var brush = new SolidBrush(fill))
            {
                e.Graphics.FillRectangle(brush, cell);
            }

            using (var border = new Pen(GridColor, 1f))
            {
                e.Graphics.DrawRectangle(border, cell);
            }

            if (_selected.Contains(node.Id))
            {
                using var selection = new Pen(HighlightColor, Math.Max(2f, geometry.CellSize / 14f));
                e.Graphics.DrawRectangle(selection, Rectangle.Inflate(cell, -1, -1));
            }

            if (contribution?.IsDestroyed == true)
            {
                using var strike = new Pen(DeadStripeColor, Math.Max(2f, geometry.CellSize / 10f));
                e.Graphics.DrawLine(
                    strike,
                    cell.Left + 5,
                    cell.Top + 5,
                    cell.Right - 5,
                    cell.Bottom - 5);
                e.Graphics.DrawLine(
                    strike,
                    cell.Right - 5,
                    cell.Top + 5,
                    cell.Left + 5,
                    cell.Bottom - 5);
            }

            if (_map.Outposts.Any(outpost => outpost.TerrainId == node.Id))
            {
                var outpost = _map.Outposts.Single(item => item.TerrainId == node.Id);
                using var outpostBrush = new SolidBrush(
                    outpost.Faction == Faction.Aurora ? AuroraColor : BastionColor);
                var size = Math.Max(5f, geometry.CellSize * 0.25f);
                var center = new PointF(cell.Left + (cell.Width / 2f), cell.Top + (cell.Height / 2f));
                var points = new[]
                {
                    new PointF(center.X, center.Y - size),
                    new PointF(center.X + size, center.Y),
                    new PointF(center.X, center.Y + size),
                    new PointF(center.X - size, center.Y)
                };
                e.Graphics.FillPolygon(outpostBrush, points);
            }

            if (geometry.CellSize >= 29 && contribution is not null)
            {
                var label = node.Id.ToString();
                var textSize = e.Graphics.MeasureString(label, font);
                using var textBrush = new SolidBrush(Color.FromArgb(220, 235, 245));
                e.Graphics.DrawString(
                    label,
                    font,
                    textBrush,
                    cell.Left + (cell.Width - textSize.Width) / 2f,
                    cell.Bottom - textSize.Height - 1);
            }
        }

        font.Dispose();
        DrawLegend(e.Graphics, geometry);
    }

    private void DrawLegend(Graphics graphics, Geometry geometry)
    {
        var y = geometry.Top + geometry.GridHeight + 12;
        if (y > ClientSize.Height - 20)
        {
            return;
        }

        using var font = new Font("Segoe UI", 8.5f);
        var x = geometry.Left;
        DrawLegendItem(graphics, font, x, y, AuroraColor, "Aurora 路径");
        x += 112;
        DrawLegendItem(graphics, font, x, y, BastionColor, "Bastion 路径");
        x += 112;
        DrawLegendItem(graphics, font, x, y, DeadColor, "已破坏");
        x += 110;
        DrawLegendItem(graphics, font, x, y, HighlightColor, "待提交");
    }

    private static void DrawLegendItem(
        Graphics graphics,
        Font font,
        int x,
        int y,
        Color color,
        string text)
    {
        using var brush = new SolidBrush(color);
        graphics.FillRectangle(brush, x, y + 2, 14, 10);
        using var textBrush = new SolidBrush(Color.FromArgb(179, 192, 207));
        graphics.DrawString(text, font, textBrush, x + 20, y - 2);
    }

    private void DrawEmpty(Graphics graphics)
    {
        using var font = new Font("Segoe UI", 11f);
        using var brush = new SolidBrush(Color.FromArgb(132, 148, 166));
        const string message = "生成或恢复场景后显示地形网格";
        var size = graphics.MeasureString(message, font);
        graphics.DrawString(
            message,
            font,
            brush,
            (ClientSize.Width - size.Width) / 2f,
            (ClientSize.Height - size.Height) / 2f);
    }

    private Geometry CalculateGeometry(TerrainMap map)
    {
        var columns = map.Nodes.Max(node => node.Column) + 1;
        var rows = map.Nodes.Max(node => node.Row) + 1;
        const int padding = 24;
        const int legendSpace = 38;
        var availableWidth = Math.Max(50, ClientSize.Width - (padding * 2));
        var availableHeight = Math.Max(50, ClientSize.Height - (padding * 2) - legendSpace);
        var cellSize = Math.Max(6, Math.Min(availableWidth / columns, availableHeight / rows));
        var gridWidth = cellSize * columns;
        var gridHeight = cellSize * rows;
        var left = (ClientSize.Width - gridWidth) / 2;
        var top = padding + Math.Max(0, (availableHeight - gridHeight) / 2);
        return new Geometry(left, top, rows, columns, cellSize, gridWidth, gridHeight);
    }

    private static Color Blend(Color color, float factor)
    {
        return Color.FromArgb(
            (int)(color.R * factor),
            (int)(color.G * factor),
            (int)(color.B * factor));
    }

    private sealed record Geometry(
        int Left,
        int Top,
        int Rows,
        int Columns,
        int CellSize,
        int GridWidth,
        int GridHeight)
    {
        public Rectangle Cell(int row, int column)
        {
            return new Rectangle(
                Left + (column * CellSize),
                Top + (row * CellSize),
                CellSize,
                CellSize);
        }
    }
}
