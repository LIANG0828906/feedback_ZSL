using System.Globalization;
using TacticalField.Core;

namespace TacticalField.WinForms;

internal sealed class MainForm : Form
{
    private readonly BattlefieldEngine _engine;
    private readonly BattlefieldPanel _battlefield;
    private readonly ComboBox _unitCombo;
    private readonly ComboBox _targetCombo;
    private readonly ComboBox _maskCombo;
    private readonly CheckBox _selectedStartCheck;
    private readonly Label _stateLabel;
    private readonly Label _pendingLabel;
    private readonly TextBox _log;
    private readonly List<BattleChange> _pending = new();
    private string? _lastPathUnit;
    private int _lastPathTarget = -1;

    public MainForm(BattlefieldEngine engine, string dataDir, bool smoke = false)
    {
        _engine = engine;
        Text = $"Tactical Field - incremental cost fields - {dataDir}";
        Width = 1280;
        Height = 820;
        MinimumSize = new Size(1000, 680);
        StartPosition = FormStartPosition.CenterScreen;

        var root = new TableLayoutPanel
        {
            Dock = DockStyle.Fill,
            ColumnCount = 2,
            RowCount = 1
        };
        root.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        root.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 350));
        Controls.Add(root);

        _battlefield = new BattlefieldPanel { Dock = DockStyle.Fill };
        root.Controls.Add(_battlefield, 0, 0);

        var sidebar = new TableLayoutPanel
        {
            Dock = DockStyle.Right,
            Padding = new Padding(12),
            BackColor = Color.FromArgb(38, 42, 52),
            ColumnCount = 1,
            RowCount = 6
        };
        sidebar.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        sidebar.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        sidebar.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        sidebar.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        sidebar.RowStyles.Add(new RowStyle(SizeType.Absolute, 54));
        sidebar.RowStyles.Add(new RowStyle(SizeType.Absolute, 24));
        sidebar.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        root.Controls.Add(sidebar, 1, 0);

        var layout = new TableLayoutPanel
        {
            Dock = DockStyle.Top,
            AutoSize = true,
            ColumnCount = 2,
            RowCount = 0
        };
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 105));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        sidebar.Controls.Add(layout, 0, 0);

        _unitCombo = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Dock = DockStyle.Fill };
        _targetCombo = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Dock = DockStyle.Fill };
        _maskCombo = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Dock = DockStyle.Fill };
        _maskCombo.Items.AddRange(new object[] { 1, 2, 3, 5, 7 });
        _maskCombo.SelectedIndex = 0;
        AddRow(layout, "Unit", _unitCombo);
        AddRow(layout, "Target", _targetCombo);
        AddRow(layout, "Unit mask", _maskCombo);

        _selectedStartCheck = new CheckBox
        {
            Text = "Query from selected cell",
            ForeColor = Color.White,
            AutoSize = true
        };
        layout.Controls.Add(new Label { Text = string.Empty }, 0, layout.RowCount++);
        layout.Controls.Add(_selectedStartCheck, 1, layout.RowCount - 1);

        var queryButton = Button("Query path", QueryPath);
        var obstacleNow = Button("Obstacle now", () => ApplyNow(cell => BattleChange.SetObstacle(cell, !_engine.Current.Grid.IsObstacle(cell))));
        var hazardNow = Button("Hazard now", () => ApplyNow(cell => BattleChange.SetHazard(cell, !_engine.Current.Grid.IsHazard(cell))));
        var stageObstacle = Button("Stage obstacle", () => Stage(cell => BattleChange.SetObstacle(cell, !_engine.Current.Grid.IsObstacle(cell))));
        var stageHazard = Button("Stage hazard", () => Stage(cell => BattleChange.SetHazard(cell, !_engine.Current.Grid.IsHazard(cell))));
        var stageMask = Button("Stage unit mask", StageMask);
        var applyBatch = Button("Apply batch", ApplyBatch);
        var clearBatch = Button("Clear batch", ClearBatch);

        var buttons = new FlowLayoutPanel
        {
            Dock = DockStyle.Top,
            AutoSize = true,
            FlowDirection = FlowDirection.LeftToRight,
            WrapContents = true,
            Padding = new Padding(0, 10, 0, 0)
        };
        buttons.Controls.AddRange(new Control[]
        {
            queryButton, obstacleNow, hazardNow, stageObstacle, stageHazard, stageMask, applyBatch, clearBatch
        });
        buttons.Dock = DockStyle.Fill;
        sidebar.Controls.Add(buttons, 0, 2);

        _stateLabel = new Label
        {
            Dock = DockStyle.Top,
            Height = 48,
            ForeColor = Color.Gainsboro,
            Padding = new Padding(0, 8, 0, 0)
        };
        _pendingLabel = new Label
        {
            Dock = DockStyle.Top,
            Height = 24,
            ForeColor = Color.Khaki,
            Text = "Pending batch: 0"
        };
        _stateLabel.AutoSize = false;
        sidebar.Controls.Add(_stateLabel, 0, 3);
        sidebar.Controls.Add(_pendingLabel, 0, 4);

        _log = new TextBox
        {
            Dock = DockStyle.Fill,
            Multiline = true,
            ReadOnly = true,
            ScrollBars = ScrollBars.Vertical,
            BackColor = Color.FromArgb(20, 22, 28),
            ForeColor = Color.Gainsboro,
            Font = new Font("Consolas", 8.5f)
        };
        sidebar.Controls.Add(_log, 0, 5);

        _unitCombo.SelectedIndexChanged += (_, _) => RebuildTargets();
        Load += (_, _) =>
        {
            _unitCombo.Items.AddRange(_engine.Current.Units.Keys.OrderBy(value => value, StringComparer.Ordinal).Cast<object>().ToArray());
            if (_unitCombo.Items.Count > 0)
            {
                _unitCombo.SelectedIndex = 0;
            }

            AppendLog($"loaded version={_engine.Current.Version} sequence={_engine.Current.BatchSequence} chain={_engine.Current.ChainHash}");
            RefreshView("initial view");
        };

        if (smoke)
        {
            var timer = new System.Windows.Forms.Timer { Interval = 1200 };
            timer.Tick += (_, _) =>
            {
                timer.Stop();
                Close();
            };
            Shown += (_, _) => timer.Start();
        }
    }

    private static void AddRow(TableLayoutPanel layout, string label, Control control)
    {
        var row = layout.RowCount++;
        layout.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        layout.Controls.Add(new Label
        {
            Text = label,
            ForeColor = Color.Gainsboro,
            AutoSize = true,
            Anchor = AnchorStyles.Left
        }, 0, row);
        layout.Controls.Add(control, 1, row);
    }

    private static Button Button(string text, Action action)
    {
        var button = new Button
        {
            Text = text,
            AutoSize = true,
            Margin = new Padding(0, 0, 6, 6)
        };
        button.Click += (_, _) => action();
        return button;
    }

    private void RebuildTargets()
    {
        _targetCombo.Items.Clear();
        if (_unitCombo.SelectedItem is not string unitId
            || !_engine.Current.Units.TryGetValue(unitId, out var unit))
        {
            return;
        }

        _targetCombo.Items.AddRange(unit.Targets.Cast<object>().ToArray());
        if (_targetCombo.Items.Count > 0)
        {
            _targetCombo.SelectedIndex = 0;
        }
    }

    private void QueryPath()
    {
        RefreshView("query");
    }

    private void RefreshView(string reason)
    {
        if (_unitCombo.SelectedItem is not string unitId
            || _targetCombo.SelectedItem is not int target)
        {
            return;
        }

        var view = _engine.Current;
        var start = _selectedStartCheck.Checked ? _battlefield.SelectedCell : (int?)null;
        if (start is < 0)
        {
            start = null;
        }

        var query = _engine.QueryPath(unitId, target, start);
        var key = new FieldKey(unitId, target);
        _battlefield.SetState(view, key, query.Code == ErrorCodes.Ok ? query.Cells : Array.Empty<int>(), _battlefield.SelectedCell);
        var field = view.Fields[key];
        _stateLabel.Text =
            $"publication v{view.Version} seq{view.BatchSequence}\n" +
            $"field v{field.FieldVersion} {field.ContentHash[..12]}\n" +
            $"query {query.Code} cache={(query.CacheHit ? "hit" : "miss")} cost={query.Cost}";
        _lastPathUnit = unitId;
        _lastPathTarget = target;
        AppendLog(
            $"{reason}: {query.Code} cache={(query.CacheHit ? "hit" : "miss")} cost={query.Cost} " +
            $"path=[{string.Join(",", query.Cells)}] path_hash={Short(query.PathHash)}");
    }

    private void ApplyNow(Func<int, BattleChange> changeFactory)
    {
        var cell = _battlefield.SelectedCell;
        if (cell < 0)
        {
            MessageBox.Show("Select a grid cell first.", "Tactical Field");
            return;
        }

        var result = _engine.SubmitBatch(new[] { changeFactory(cell) });
        LogBatch(result, $"local cell={cell}");
        RefreshView("local change");
    }

    private void Stage(Func<int, BattleChange> changeFactory)
    {
        var cell = _battlefield.SelectedCell;
        if (cell < 0)
        {
            MessageBox.Show("Select a grid cell first.", "Tactical Field");
            return;
        }

        _pending.Add(changeFactory(cell));
        UpdatePending();
    }

    private void StageMask()
    {
        if (_unitCombo.SelectedItem is not string unitId || _maskCombo.SelectedItem is not int mask)
        {
            return;
        }

        _pending.Add(BattleChange.SetUnitMask(unitId, mask));
        UpdatePending();
    }

    private void ApplyBatch()
    {
        if (_pending.Count == 0)
        {
            MessageBox.Show("Stage at least one change first.", "Tactical Field");
            return;
        }

        var changes = _pending.ToArray();
        var result = _engine.SubmitBatch(changes);
        LogBatch(result, $"batch changes={changes.Length}");
        if (result.IsSuccess)
        {
            _pending.Clear();
            UpdatePending();
            RefreshView("batch applied");
        }
    }

    private void ClearBatch()
    {
        _pending.Clear();
        UpdatePending();
    }

    private void UpdatePending()
    {
        _pendingLabel.Text = $"Pending batch: {_pending.Count}";
        if (_pending.Count > 0)
        {
            AppendLog("pending " + string.Join(";", _pending.Select(change => change.Canonical())));
        }
    }

    private void LogBatch(BatchResult result, string label)
    {
        AppendLog(
            $"{label}: {result.Code} v{result.PublicationVersion} seq{result.BatchSequence} " +
            $"deltas={result.Deltas.Length} chain={Short(result.ChainHash)}");
        foreach (var delta in result.Deltas)
        {
            AppendLog(
                $"  {delta.Key} impact={delta.ImpactCells.Length} recomputed={delta.RecomputedCells.Length} " +
                $"reused={delta.ReusedCells} {Short(delta.OldHash)}->{Short(delta.NewHash)}");
        }

        if (!result.IsSuccess)
        {
            MessageBox.Show(result.Message ?? result.Code, "Tactical Field", MessageBoxButtons.OK, MessageBoxIcon.Warning);
        }
    }

    private void AppendLog(string message)
    {
        _log.AppendText(
            DateTime.Now.ToString("HH:mm:ss", CultureInfo.InvariantCulture) + " " + message + Environment.NewLine);
    }

    private static string Short(string value) => value.Length <= 12 ? value : value[..12];
}
