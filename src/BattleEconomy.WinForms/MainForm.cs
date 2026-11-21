using System.Collections.Immutable;
using BattleEconomy.Core;

namespace BattleEconomy.WinForms;

public sealed class MainForm : Form
{
    private static readonly Color PageColor = Color.FromArgb(12, 18, 26);
    private static readonly Color PanelColor = Color.FromArgb(22, 30, 41);
    private static readonly Color RaisedColor = Color.FromArgb(31, 42, 56);
    private static readonly Color TextColor = Color.FromArgb(226, 234, 243);
    private static readonly Color MutedColor = Color.FromArgb(151, 167, 183);
    private static readonly Color AuroraColor = Color.FromArgb(76, 201, 176);
    private static readonly Color BastionColor = Color.FromArgb(242, 166, 90);
    private static readonly Color AccentColor = Color.FromArgb(88, 166, 255);

    private readonly string _workbenchRoot;
    private readonly ComboBox _profileCombo = new();
    private readonly NumericUpDown _seedInput = new();
    private readonly NumericUpDown _rowsInput = new();
    private readonly NumericUpDown _columnsInput = new();
    private readonly NumericUpDown _densityInput = new();
    private readonly Button _generateButton = new();
    private readonly Button _loadProfileButton = new();
    private readonly Button _loadSampleButton = new();
    private readonly Button _submitButton = new();
    private readonly Button _cancelButton = new();
    private readonly Button _recoverButton = new();
    private readonly Label _versionLabel = new();
    private readonly Label _auroraLabel = new();
    private readonly Label _bastionLabel = new();
    private readonly Label _batchLabel = new();
    private readonly Label _closureLabel = new();
    private readonly Label _selectionLabel = new();
    private readonly Label _statusLabel = new();
    private readonly Label _logPathLabel = new();
    private readonly ProgressBar _progressBar = new();
    private readonly TerrainGridControl _terrainGrid = new();
    private readonly EconomyChartControl _chart = new();
    private readonly TextBox _activityBox = new();
    private readonly List<EconomyChartPoint> _history = [];
    private readonly Dictionary<int, string> _selected = [];

    private ScenarioBundle? _bundle;
    private EconomyEngine? _engine;
    private TrajectoryLog? _trajectory;
    private CancellationTokenSource? _settlementCancellation;
    private bool _busy;

    public MainForm()
    {
        Text = "BattleEconomy Replay Console";
        StartPosition = FormStartPosition.CenterScreen;
        MinimumSize = new Size(1180, 760);
        Size = new Size(1460, 900);
        BackColor = PageColor;
        ForeColor = TextColor;
        Font = new Font("Segoe UI", 9f);
        _workbenchRoot = Path.Combine(Directory.GetCurrentDirectory(), "local-data", "workbench");

        BuildLayout();
        Shown += (_, _) => LoadProfile(ScenarioCatalog.BuiltIns[1]);
        FormClosed += (_, _) =>
        {
            _settlementCancellation?.Cancel();
            _settlementCancellation?.Dispose();
            _trajectory?.Dispose();
        };
    }

    private void BuildLayout()
    {
        var root = new TableLayoutPanel
        {
            Dock = DockStyle.Fill,
            RowCount = 3,
            ColumnCount = 1,
            BackColor = PageColor,
            Padding = new Padding(0)
        };
        root.RowStyles.Add(new RowStyle(SizeType.Absolute, 88));
        root.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        root.RowStyles.Add(new RowStyle(SizeType.Absolute, 126));
        Controls.Add(root);

        root.Controls.Add(BuildHeader(), 0, 0);
        root.Controls.Add(BuildContent(), 0, 1);
        root.Controls.Add(BuildFooter(), 0, 2);
        ConfigureInputs();
    }

    private Control BuildHeader()
    {
        var panel = new Panel
        {
            Dock = DockStyle.Fill,
            BackColor = Color.FromArgb(16, 23, 33),
            Padding = new Padding(24, 14, 20, 10)
        };
        var title = new Label
        {
            AutoSize = true,
            Text = "赛事经济复核台",
            Font = new Font("Segoe UI Semibold", 18f),
            ForeColor = TextColor,
            Location = new Point(24, 13)
        };
        var subtitle = new Label
        {
            AutoSize = true,
            Text = "地形破坏 · 增量采集路径 · 不可变版本 · 原子恢复",
            Font = new Font("Segoe UI", 8.7f),
            ForeColor = MutedColor,
            Location = new Point(27, 52)
        };
        panel.Controls.Add(title);
        panel.Controls.Add(subtitle);

        var controls = new FlowLayoutPanel
        {
            AutoSize = true,
            FlowDirection = FlowDirection.LeftToRight,
            WrapContents = false,
            Anchor = AnchorStyles.Top | AnchorStyles.Right,
            Location = new Point(420, 18),
            Height = 58
        };
        _profileCombo.DropDownStyle = ComboBoxStyle.DropDownList;
        _profileCombo.Width = 142;
        _profileCombo.Items.AddRange(ScenarioCatalog.BuiltIns.Select(item => (object)item.Name).ToArray());
        _profileCombo.SelectedIndex = 1;
        _loadProfileButton.Text = "载入档案";
        StyleButton(_loadProfileButton, RaisedColor);
        controls.Controls.Add(Field("场景", _profileCombo, 154));
        controls.Controls.Add(Field("种子", _seedInput, 82));
        controls.Controls.Add(Field("行", _rowsInput, 62));
        controls.Controls.Add(Field("列", _columnsInput, 62));
        controls.Controls.Add(Field("密度%", _densityInput, 68));
        controls.Controls.Add(Spacer(6));
        controls.Controls.Add(_loadProfileButton);
        controls.Controls.Add(Spacer(4));
        _generateButton.Text = "生成 / 恢复";
        StyleButton(_generateButton, AccentColor, Color.White);
        controls.Controls.Add(_generateButton);
        panel.Controls.Add(controls);
        panel.Resize += (_, _) => controls.Left = Math.Max(420, panel.ClientSize.Width - controls.Width - 24);
        _loadProfileButton.Click += (_, _) => LoadSelectedProfile();
        _generateButton.Click += (_, _) => GenerateFromInputs();
        return panel;
    }

    private Control BuildContent()
    {
        var split = new SplitContainer
        {
            Dock = DockStyle.Fill,
            Orientation = Orientation.Vertical,
            SplitterWidth = 7,
            Size = new Size(1400, 720),
            BackColor = PageColor,
            Panel1MinSize = 640,
            Panel2MinSize = 340
        };
        split.SplitterDistance = 820;
        split.Panel1.Padding = new Padding(18, 14, 8, 8);
        split.Panel1.Controls.Add(BuildTerrainPanel());
        split.Panel2.Padding = new Padding(8, 14, 18, 8);
        split.Panel2.Controls.Add(BuildSidePanel());
        return split;
    }

    private Control BuildTerrainPanel()
    {
        var panel = new Panel
        {
            Dock = DockStyle.Fill,
            BackColor = PanelColor,
            Padding = new Padding(1)
        };
        var caption = new Label
        {
            Dock = DockStyle.Top,
            Height = 36,
            Text = "  战场地形 / 点击地块加入或移出待破坏批次",
            TextAlign = ContentAlignment.MiddleLeft,
            Font = new Font("Segoe UI Semibold", 10f),
            ForeColor = TextColor,
            BackColor = PanelColor
        };
        _terrainGrid.Dock = DockStyle.Fill;
        _terrainGrid.BackColor = Color.FromArgb(18, 25, 35);
        _terrainGrid.TileClicked += TileClicked;
        panel.Controls.Add(_terrainGrid);
        panel.Controls.Add(caption);
        return panel;
    }

    private Control BuildSidePanel()
    {
        var layout = new TableLayoutPanel
        {
            Dock = DockStyle.Fill,
            ColumnCount = 1,
            RowCount = 3,
            BackColor = PageColor
        };
        layout.RowStyles.Add(new RowStyle(SizeType.Absolute, 176));
        layout.RowStyles.Add(new RowStyle(SizeType.Percent, 42));
        layout.RowStyles.Add(new RowStyle(SizeType.Percent, 58));

        var economyPanel = new Panel
        {
            Dock = DockStyle.Fill,
            BackColor = PanelColor,
            Padding = new Padding(16, 12, 16, 10),
            Margin = new Padding(0, 0, 0, 10)
        };
        var economyTitle = new Label
        {
            AutoSize = true,
            Text = "已发布经济水位",
            Font = new Font("Segoe UI Semibold", 10f),
            ForeColor = TextColor,
            Location = new Point(16, 12)
        };
        _versionLabel.AutoSize = false;
        _versionLabel.Location = new Point(16, 40);
        _versionLabel.Size = new Size(440, 34);
        _versionLabel.ForeColor = MutedColor;
        _versionLabel.Font = new Font("Consolas", 8.4f);
        _auroraLabel.AutoSize = true;
        _auroraLabel.Location = new Point(16, 82);
        _auroraLabel.ForeColor = AuroraColor;
        _auroraLabel.Font = new Font("Segoe UI Semibold", 10f);
        _bastionLabel.AutoSize = true;
        _bastionLabel.Location = new Point(16, 108);
        _bastionLabel.ForeColor = BastionColor;
        _bastionLabel.Font = new Font("Segoe UI Semibold", 10f);
        _batchLabel.AutoSize = true;
        _batchLabel.Location = new Point(16, 140);
        _batchLabel.ForeColor = MutedColor;
        _closureLabel.AutoSize = true;
        _closureLabel.Location = new Point(170, 140);
        _closureLabel.ForeColor = AccentColor;
        economyPanel.Controls.Add(economyTitle);
        economyPanel.Controls.Add(_versionLabel);
        economyPanel.Controls.Add(_auroraLabel);
        economyPanel.Controls.Add(_bastionLabel);
        economyPanel.Controls.Add(_batchLabel);
        economyPanel.Controls.Add(_closureLabel);

        _chart.Dock = DockStyle.Fill;
        _chart.Margin = new Padding(0, 0, 0, 10);

        var logPanel = new Panel
        {
            Dock = DockStyle.Fill,
            BackColor = PanelColor,
            Padding = new Padding(12, 8, 12, 10)
        };
        var logTitle = new Label
        {
            Dock = DockStyle.Top,
            Height = 28,
            Text = "本地轨迹与校验记录",
            Font = new Font("Segoe UI Semibold", 10f),
            ForeColor = TextColor
        };
        _activityBox.Dock = DockStyle.Fill;
        _activityBox.Multiline = true;
        _activityBox.ReadOnly = true;
        _activityBox.ScrollBars = ScrollBars.Vertical;
        _activityBox.BackColor = Color.FromArgb(15, 21, 30);
        _activityBox.ForeColor = Color.FromArgb(188, 204, 220);
        _activityBox.BorderStyle = BorderStyle.None;
        _activityBox.Font = new Font("Consolas", 8.4f);
        logPanel.Controls.Add(_activityBox);
        logPanel.Controls.Add(logTitle);

        layout.Controls.Add(economyPanel, 0, 0);
        layout.Controls.Add(_chart, 0, 1);
        layout.Controls.Add(logPanel, 0, 2);
        return layout;
    }

    private Control BuildFooter()
    {
        var panel = new Panel
        {
            Dock = DockStyle.Fill,
            BackColor = Color.FromArgb(16, 23, 33),
            Padding = new Padding(24, 12, 24, 10)
        };
        _selectionLabel.AutoSize = true;
        _selectionLabel.Location = new Point(24, 16);
        _selectionLabel.ForeColor = TextColor;
        _loadSampleButton.Text = "载入下一条样本破坏";
        _submitButton.Text = "提交并统一结算";
        _cancelButton.Text = "协作式取消";
        _recoverButton.Text = "恢复未完成批次";
        StyleButton(_loadSampleButton, RaisedColor);
        StyleButton(_submitButton, AccentColor, Color.White);
        StyleButton(_cancelButton, Color.FromArgb(96, 50, 58));
        StyleButton(_recoverButton, Color.FromArgb(58, 77, 70));
        _loadSampleButton.Location = new Point(24, 48);
        _submitButton.Location = new Point(190, 48);
        _cancelButton.Location = new Point(350, 48);
        _recoverButton.Location = new Point(462, 48);
        _cancelButton.Enabled = false;
        _recoverButton.Enabled = false;
        _loadSampleButton.Click += (_, _) => LoadNextSampleBatch();
        _submitButton.Click += async (_, _) => await SubmitSelectedBatchAsync();
        _cancelButton.Click += (_, _) => RequestCancellation();
        _recoverButton.Click += async (_, _) => await ResumePendingBatchAsync();

        _progressBar.Location = new Point(594, 49);
        _progressBar.Size = new Size(150, 18);
        _progressBar.Style = ProgressBarStyle.Continuous;
        _progressBar.Minimum = 0;
        _progressBar.Maximum = 1;
        _statusLabel.AutoSize = false;
        _statusLabel.Location = new Point(760, 45);
        _statusLabel.Size = new Size(620, 42);
        _statusLabel.ForeColor = MutedColor;
        _statusLabel.Text = "等待场景";
        _statusLabel.Anchor = AnchorStyles.Top | AnchorStyles.Left | AnchorStyles.Right;
        _logPathLabel.AutoSize = false;
        _logPathLabel.Location = new Point(24, 96);
        _logPathLabel.Size = new Size(1320, 18);
        _logPathLabel.ForeColor = Color.FromArgb(109, 128, 147);
        _logPathLabel.Font = new Font("Consolas", 8f);
        _logPathLabel.Anchor = AnchorStyles.Top | AnchorStyles.Left | AnchorStyles.Right;

        panel.Controls.Add(_selectionLabel);
        panel.Controls.Add(_loadSampleButton);
        panel.Controls.Add(_submitButton);
        panel.Controls.Add(_cancelButton);
        panel.Controls.Add(_recoverButton);
        panel.Controls.Add(_progressBar);
        panel.Controls.Add(_statusLabel);
        panel.Controls.Add(_logPathLabel);
        panel.Resize += (_, _) =>
        {
            _statusLabel.Width = Math.Max(200, panel.ClientSize.Width - _statusLabel.Left - 24);
            _logPathLabel.Width = Math.Max(200, panel.ClientSize.Width - 48);
        };
        return panel;
    }

    private void ConfigureInputs()
    {
        _seedInput.Minimum = 0;
        _seedInput.Maximum = int.MaxValue;
        _seedInput.Value = 2202;
        _rowsInput.Minimum = 4;
        _rowsInput.Maximum = 48;
        _rowsInput.Value = 18;
        _columnsInput.Minimum = 4;
        _columnsInput.Maximum = 48;
        _columnsInput.Value = 14;
        _densityInput.Minimum = 0;
        _densityInput.Maximum = 100;
        _densityInput.Value = 55;
        foreach (var input in new[] { _seedInput, _rowsInput, _columnsInput, _densityInput })
        {
            input.ThousandsSeparator = false;
            input.TextAlign = HorizontalAlignment.Right;
        }
    }

    private static Control Field(string label, Control input, int width)
    {
        var panel = new Panel
        {
            Width = width + 4,
            Height = 56,
            Margin = new Padding(0, 0, 4, 0)
        };
        var caption = new Label
        {
            AutoSize = true,
            Text = label,
            ForeColor = MutedColor,
            Font = new Font("Segoe UI", 7.8f),
            Location = new Point(0, 0)
        };
        input.Location = new Point(0, 19);
        input.Width = width;
        panel.Controls.Add(caption);
        panel.Controls.Add(input);
        return panel;
    }

    private static Control Spacer(int width)
    {
        return new Panel { Width = width, Height = 24, Margin = Padding.Empty };
    }

    private static void StyleButton(Button button, Color color, Color? textColor = null)
    {
        button.AutoSize = false;
        button.Height = 29;
        button.Width = Math.Max(92, button.Text.Length * 14);
        button.FlatStyle = FlatStyle.Flat;
        button.FlatAppearance.BorderSize = 0;
        button.BackColor = color;
        button.ForeColor = textColor ?? TextColor;
        button.Cursor = Cursors.Hand;
        button.Font = new Font("Segoe UI Semibold", 8.8f);
        button.Margin = new Padding(0, 17, 4, 0);
    }

    private static ScenarioSpec? FindBuiltIn(string? name)
    {
        return ScenarioCatalog.BuiltIns.FirstOrDefault(item => item.Name == name);
    }

    private void LoadSelectedProfile()
    {
        var spec = FindBuiltIn(_profileCombo.SelectedItem as string);
        if (spec is not null)
        {
            LoadProfile(spec);
        }
    }

    private void LoadProfile(ScenarioSpec spec)
    {
        if (_busy)
        {
            return;
        }

        _profileCombo.SelectedItem = spec.Name;
        _seedInput.Value = spec.Seed;
        _rowsInput.Value = spec.Rows;
        _columnsInput.Value = spec.Columns;
        _densityInput.Value = spec.AdjacencyDensity;
        GenerateFromInputs();
    }

    private void GenerateFromInputs()
    {
        if (_busy)
        {
            return;
        }

        try
        {
            var spec = new ScenarioSpec(
                _profileCombo.SelectedItem as string ?? "custom",
                (int)_seedInput.Value,
                (int)_rowsInput.Value,
                (int)_columnsInput.Value,
                (int)_densityInput.Value,
                BatchCount: 8,
                DestructionsPerBatch: 3);
            var bundle = DeterministicScenarioGenerator.Generate(spec);
            var key = $"seed{spec.Seed}-r{spec.Rows}c{spec.Columns}-d{spec.AdjacencyDensity}";
            var directory = Path.Combine(_workbenchRoot, key);
            Directory.CreateDirectory(directory);
            DeterministicScenarioGenerator.WriteScenario(bundle, directory);

            _trajectory?.Dispose();
            _trajectory = new TrajectoryLog(Path.Combine(directory, "trajectory.jsonl"), append: true);
            var store = new EconomySnapshotStore(Path.Combine(directory, "snapshots"));
            var opened = EconomyEngine.OpenOrCreate(bundle.Terrain, store, _trajectory);
            if (!opened.Success || opened.Engine is null)
            {
                _trajectory.Dispose();
                _trajectory = null;
                MessageBox.Show(
                    this,
                    $"{opened.Error?.Code}\n{opened.Error?.Message}",
                    "场景打开失败",
                    MessageBoxButtons.OK,
                    MessageBoxIcon.Error);
                return;
            }

            _bundle = bundle;
            _engine = opened.Engine;
            _selected.Clear();
            var snapshot = _engine.AcquirePublishedSnapshot().Snapshot;
            _history.Clear();
            AddHistoryPoint(snapshot);
            RefreshPublished(snapshot);
            SetClosureState("闭包未运行", AccentColor);
            _recoverButton.Enabled = opened.PendingBatch is not null;
            _activityBox.Clear();
            AppendActivity($"scenario  {spec.Name}  seed={spec.Seed}  {spec.Rows}x{spec.Columns}  density={spec.AdjacencyDensity}%");
            AppendActivity($"baseline  version={snapshot.Version}  hash={snapshot.ContentHash}");
            AppendActivity($"outposts  {string.Join(", ", _engine.Map.Outposts.Select(item => $"{item.Id}:{item.Faction}@{item.TerrainId}"))}");
            if (opened.PendingBatch is not null)
            {
                AppendActivity($"recoverable batch {opened.PendingBatch.BatchNumber} with {opened.PendingBatch.Destructions.Length} destructions");
                _statusLabel.Text = "检测到未完成批次，可从已提交快照恢复";
            }
            else
            {
                _statusLabel.Text = "场景已就绪";
            }

            _logPathLabel.Text = $"trajectory: {_trajectory.Path}";
            _selectionLabel.Text = "待破坏：0 块";
            _progressBar.Maximum = Math.Max(1, _bundle.Batches.Length);
            _progressBar.Value = Math.Min(_progressBar.Maximum, (int)Math.Min(int.MaxValue, snapshot.BatchSequence));
        }
        catch (Exception exception)
        {
            MessageBox.Show(
                this,
                exception.Message,
                "场景生成失败",
                MessageBoxButtons.OK,
                MessageBoxIcon.Error);
        }
    }

    private void TileClicked(int terrainId)
    {
        if (_busy || _engine is null)
        {
            return;
        }

        var snapshot = _engine.AcquirePublishedSnapshot().Snapshot;
        if (!snapshot.Contributions.TryGetValue(terrainId, out var contribution) ||
            contribution.IsDestroyed)
        {
            _statusLabel.Text = $"地形 {terrainId} 已破坏，不能重复提交";
            return;
        }

        if (_selected.Remove(terrainId))
        {
            _statusLabel.Text = $"地块 {terrainId} 已移出批次";
        }
        else
        {
            var batchNumber = snapshot.BatchSequence + 1;
            _selected[terrainId] = $"UI-B{batchNumber:D4}-T{terrainId:D4}";
            _statusLabel.Text = $"地块 {terrainId} 已加入批次 {batchNumber}";
        }

        _terrainGrid.SetState(_engine.Map, snapshot, _selected.Keys);
        UpdateSelectionLabel();
    }

    private void LoadNextSampleBatch()
    {
        if (_busy || _engine is null || _bundle is null)
        {
            return;
        }

        var snapshot = _engine.AcquirePublishedSnapshot().Snapshot;
        var index = (int)Math.Clamp(snapshot.BatchSequence, 0, int.MaxValue);
        if (index >= _bundle.Batches.Length)
        {
            _statusLabel.Text = "内置破坏序列已全部提交";
            return;
        }

        var sample = _bundle.Batches[index];
        _selected.Clear();
        foreach (var destruction in sample.Destructions)
        {
            if (snapshot.Contributions.TryGetValue(destruction.TerrainId, out var contribution) &&
                !contribution.IsDestroyed)
            {
                _selected[destruction.TerrainId] = destruction.Key;
            }
        }

        _terrainGrid.SetState(_engine.Map, snapshot, _selected.Keys);
        UpdateSelectionLabel();
        _statusLabel.Text = $"已载入样本批次 {sample.BatchNumber}，共 {_selected.Count} 条破坏";
    }

    private async Task SubmitSelectedBatchAsync()
    {
        if (_busy || _engine is null)
        {
            return;
        }

        var snapshot = _engine.AcquirePublishedSnapshot().Snapshot;
        if (_selected.Count == 0)
        {
            _statusLabel.Text = "请先在地形网格选择地块或载入样本批次";
            return;
        }

        var batchNumber = snapshot.BatchSequence + 1;
        var destructions = _selected
            .OrderBy(item => item.Key)
            .Select(item => new DestructionEvent(item.Value, item.Key))
            .ToImmutableArray();
        var request = new BatchRequest
        {
            BatchNumber = batchNumber,
            EconomicSteps = 1,
            Destructions = destructions
        };
        await RunSettlementAsync(
            cancellationToken => _engine.SettleBatchAsync(request, cancellationToken),
            $"提交批次 {batchNumber}");
    }

    private async Task ResumePendingBatchAsync()
    {
        if (_busy || _engine is null)
        {
            return;
        }

        await RunSettlementAsync(
            cancellationToken => _engine.ResumePendingBatchAsync(cancellationToken),
            "恢复未完成批次");
    }

    private async Task RunSettlementAsync(
        Func<CancellationToken, Task<BatchResult>> operation,
        string description)
    {
        _busy = true;
        SetBusyControls(true);
        _settlementCancellation = new CancellationTokenSource();
        _statusLabel.Text = $"{description}：结算中…";
        _progressBar.Style = ProgressBarStyle.Marquee;
        _progressBar.MarqueeAnimationSpeed = 28;
        try
        {
            var result = await operation(_settlementCancellation.Token);
            if (result.Success)
            {
                if (result.NoOp)
                {
                    _statusLabel.Text = "批次中的破坏均已提交，未生成新版本";
                    AppendActivity("batch skipped: all destructs already committed");
                }
                else if (result.Trace is { } trace && result.Snapshot is { } snapshot)
                {
                    _selected.Clear();
                    AddHistoryPoint(snapshot);
                    RefreshPublished(snapshot);
                    SetClosureState(
                        trace.ClosureValidated ? "闭包 MATCH" : "闭包 MISMATCH",
                        trace.ClosureValidated ? AuroraColor : Color.FromArgb(231, 93, 103));
                    AppendActivity(
                        $"batch {trace.BatchNumber}  destructs={trace.DestructionCount}  " +
                        $"impact={trace.MergedImpactSet.Length}  reused={trace.ReusedTiles}  recomputed={trace.RecomputedTiles}");
                    AppendActivity(
                        $"closure  MATCH  version={snapshot.Version}  step={snapshot.EconomicStep}  hash={snapshot.ContentHash}");
                    if (trace.ConservativeFallback)
                    {
                        AppendActivity($"conservative fallback: {trace.ConservativeReason}");
                    }

                    _statusLabel.Text =
                        $"批次 {trace.BatchNumber} 已发布：影响 {trace.MergedImpactSet.Length} 块，复用 {trace.ReusedTiles} 块";
                    _progressBar.Value = Math.Min(
                        _progressBar.Maximum,
                        (int)Math.Min(int.MaxValue, snapshot.BatchSequence));
                }
            }
            else
            {
                var error = result.Error;
                _statusLabel.Text =
                    $"{error?.Code} · {error?.Phase ?? "-"} · index={error?.ProcessedDestructionIndex?.ToString() ?? "-"}";
                SetClosureState("未发布 / 已回滚", Color.FromArgb(231, 93, 103));
                AppendActivity(
                    $"batch failed  code={error?.Code}  phase={error?.Phase}  index={error?.ProcessedDestructionIndex}  rolledBack=true");
                if (_engine is not null)
                {
                    RefreshPublished(_engine.AcquirePublishedSnapshot().Snapshot);
                }

                MessageBox.Show(
                    this,
                    $"{error?.Code}\n阶段：{error?.Phase}\n破坏序号：{error?.ProcessedDestructionIndex?.ToString() ?? "-"}\n{error?.Message}",
                    "批次已整体回滚",
                    MessageBoxButtons.OK,
                    MessageBoxIcon.Warning);
            }
        }
        catch (Exception exception)
        {
            _statusLabel.Text = $"执行异常：{exception.GetType().Name}";
            AppendActivity($"ui exception: {exception.Message}");
        }
        finally
        {
            _busy = false;
            _settlementCancellation.Dispose();
            _settlementCancellation = null;
            _progressBar.Style = ProgressBarStyle.Continuous;
            _progressBar.MarqueeAnimationSpeed = 0;
            SetBusyControls(false);
            _recoverButton.Enabled = _engine?.PendingBatch is not null;
            UpdateSelectionLabel();
        }
    }

    private void RequestCancellation()
    {
        if (_settlementCancellation is null || _settlementCancellation.IsCancellationRequested)
        {
            return;
        }

        _settlementCancellation.Cancel();
        _statusLabel.Text = "已请求协作式取消，等待结算点退出…";
    }

    private void RefreshPublished(EconomySnapshot snapshot)
    {
        if (_engine is null)
        {
            return;
        }

        _terrainGrid.SetState(_engine.Map, snapshot, _selected.Keys);
        _versionLabel.Text =
            $"version {snapshot.Version}  step {snapshot.EconomicStep}  batch {snapshot.BatchSequence}\n" +
            $"hash {snapshot.ContentHash}";
        var aurora = snapshot.Faction(Faction.Aurora);
        var bastion = snapshot.Faction(Faction.Bastion);
        _auroraLabel.Text =
            $"Aurora   产出/步 {aurora.YieldPerStep:0.##}   累计 {aurora.TotalResources:0.##}";
        _bastionLabel.Text =
            $"Bastion  产出/步 {bastion.YieldPerStep:0.##}   累计 {bastion.TotalResources:0.##}";
        _batchLabel.Text =
            $"提交水位 {snapshot.CommitWatermark} / 样本 {_bundle?.Batches.Length ?? 0}";
        _chart.SetPoints(_history);
        UpdateSelectionLabel();
    }

    private void AddHistoryPoint(EconomySnapshot snapshot)
    {
        _history.Add(new EconomyChartPoint(
            snapshot.EconomicStep,
            snapshot.Faction(Faction.Aurora).TotalResources,
            snapshot.Faction(Faction.Bastion).TotalResources));
        _chart.SetPoints(_history);
    }

    private void SetClosureState(string text, Color color)
    {
        _closureLabel.Text = text;
        _closureLabel.ForeColor = color;
    }

    private void UpdateSelectionLabel()
    {
        var hash = _engine?.QueryCurrent().ContentHash ?? string.Empty;
        _selectionLabel.Text =
            $"待破坏：{_selected.Count} 块   当前版本：{_engine?.CurrentVersion ?? -1}   " +
            $"哈希：{(hash.Length >= 12 ? hash[..12] : hash)}";
    }

    private void SetBusyControls(bool busy)
    {
        _generateButton.Enabled = !busy;
        _loadProfileButton.Enabled = !busy;
        _loadSampleButton.Enabled = !busy;
        _submitButton.Enabled = !busy;
        _recoverButton.Enabled = !busy && _engine?.PendingBatch is not null;
        _cancelButton.Enabled = busy;
        _profileCombo.Enabled = !busy;
        _seedInput.Enabled = !busy;
        _rowsInput.Enabled = !busy;
        _columnsInput.Enabled = !busy;
        _densityInput.Enabled = !busy;
    }

    private void AppendActivity(string line)
    {
        _activityBox.AppendText($"{DateTime.Now:HH:mm:ss}  {line}{Environment.NewLine}");
    }
}
