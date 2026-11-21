using System.Drawing.Drawing2D;
using BattleEconomy.Core;

namespace BattleEconomy.WinForms;

public sealed record EconomyChartPoint(long EconomicStep, decimal Aurora, decimal Bastion);

public sealed class EconomyChartControl : Control
{
    private static readonly Color PanelColor = Color.FromArgb(22, 30, 41);
    private static readonly Color GridColor = Color.FromArgb(45, 58, 73);
    private static readonly Color AxisColor = Color.FromArgb(89, 106, 125);
    private static readonly Color AuroraColor = Color.FromArgb(76, 201, 176);
    private static readonly Color BastionColor = Color.FromArgb(242, 166, 90);
    private readonly List<EconomyChartPoint> _points = [];

    public EconomyChartControl()
    {
        DoubleBuffered = true;
        ResizeRedraw = true;
        BackColor = PanelColor;
        MinimumSize = new Size(240, 170);
    }

    public void SetPoints(IEnumerable<EconomyChartPoint> points)
    {
        _points.Clear();
        _points.AddRange(points);
        Invalidate();
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        using var background = new SolidBrush(PanelColor);
        e.Graphics.FillRectangle(background, ClientRectangle);
        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        using var titleFont = new Font("Segoe UI Semibold", 10f);
        using var labelFont = new Font("Segoe UI", 8f);
        using var textBrush = new SolidBrush(Color.FromArgb(219, 229, 240));
        e.Graphics.DrawString("阵营累计产出", titleFont, textBrush, 14, 10);

        var chart = new Rectangle(48, 40, Math.Max(50, ClientSize.Width - 66), Math.Max(50, ClientSize.Height - 72));
        using (var gridPen = new Pen(GridColor, 1f))
        {
            for (var index = 0; index <= 4; index++)
            {
                var y = chart.Top + (chart.Height * index / 4f);
                e.Graphics.DrawLine(gridPen, chart.Left, y, chart.Right, y);
            }
        }

        using (var axisPen = new Pen(AxisColor, 1.2f))
        {
            e.Graphics.DrawLine(axisPen, chart.Left, chart.Bottom, chart.Right, chart.Bottom);
            e.Graphics.DrawLine(axisPen, chart.Left, chart.Top, chart.Left, chart.Bottom);
        }

        if (_points.Count == 0)
        {
            using var emptyBrush = new SolidBrush(Color.FromArgb(122, 139, 157));
            e.Graphics.DrawString("等待首个经济版本", labelFont, emptyBrush, chart.Left + 12, chart.Top + 14);
            return;
        }

        var maximum = Math.Max(
            1m,
            _points.Max(point => Math.Max(point.Aurora, point.Bastion))) * 1.08m;
        var maxStep = Math.Max(1, _points.Max(point => point.EconomicStep));
        DrawSeries(e.Graphics, chart, _points.Select(item => (item.EconomicStep, item.Aurora)), maximum, maxStep, AuroraColor);
        DrawSeries(e.Graphics, chart, _points.Select(item => (item.EconomicStep, item.Bastion)), maximum, maxStep, BastionColor);

        using var labelBrush = new SolidBrush(Color.FromArgb(159, 173, 188));
        e.Graphics.DrawString(FormatCompact(maximum), labelFont, labelBrush, 4, chart.Top - 5);
        e.Graphics.DrawString("0", labelFont, labelBrush, 28, chart.Bottom - 7);
        e.Graphics.DrawString($"经济步 {maxStep}", labelFont, labelBrush, chart.Right - 58, chart.Bottom + 8);

        var legendX = chart.Right - 154;
        DrawLegend(e.Graphics, labelFont, legendX, 14, AuroraColor, "Aurora");
        DrawLegend(e.Graphics, labelFont, legendX + 74, 14, BastionColor, "Bastion");
    }

    private static void DrawSeries(
        Graphics graphics,
        Rectangle chart,
        IEnumerable<(long Step, decimal Value)> points,
        decimal maximum,
        long maxStep,
        Color color)
    {
        var values = points.ToArray();
        if (values.Length == 0)
        {
            return;
        }

        var mapped = values.Select(point => new PointF(
            chart.Left + (chart.Width * point.Step / (float)maxStep),
            chart.Bottom - (chart.Height * (float)(point.Value / maximum)))).ToArray();
        using var pen = new Pen(color, 2.2f)
        {
            LineJoin = LineJoin.Round
        };
        if (mapped.Length > 1)
        {
            graphics.DrawLines(pen, mapped);
        }

        using var dot = new SolidBrush(color);
        foreach (var point in mapped)
        {
            graphics.FillEllipse(dot, point.X - 3f, point.Y - 3f, 6f, 6f);
        }
    }

    private static void DrawLegend(
        Graphics graphics,
        Font font,
        int x,
        int y,
        Color color,
        string text)
    {
        using var pen = new Pen(color, 2f);
        graphics.DrawLine(pen, x, y + 7, x + 14, y + 7);
        using var brush = new SolidBrush(Color.FromArgb(198, 210, 223));
        graphics.DrawString(text, font, brush, x + 19, y);
    }

    private static string FormatCompact(decimal value)
    {
        if (value >= 1_000_000m)
        {
            return $"{value / 1_000_000m:0.#}m";
        }

        if (value >= 1_000m)
        {
            return $"{value / 1_000m:0.#}k";
        }

        return value.ToString("0.#");
    }
}
