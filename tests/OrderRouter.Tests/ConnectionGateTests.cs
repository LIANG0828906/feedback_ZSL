using OrderRouter;
using Xunit;

namespace OrderRouter.Tests;

public class ConnectionGateTests
{
    [Fact]
    public async Task Exhaustion_returns_ECONN_after_timeout()
    {
        var gate = new ConnectionGate(2, 1, 200);
        var a = await gate.AcquireAsync(QueryClass.Interactive);
        var b = await gate.AcquireAsync(QueryClass.Interactive);
        var sw = System.Diagnostics.Stopwatch.StartNew();
        var ex = await Assert.ThrowsAsync<ConnPoolException>(() => gate.AcquireAsync(QueryClass.Interactive));
        sw.Stop();
        Assert.Equal("E_CONN", ConnPoolException.Code);
        Assert.InRange(sw.ElapsedMilliseconds, 150, 5000);
        await a.DisposeAsync();
        var c = await gate.AcquireAsync(QueryClass.Interactive);
        await b.DisposeAsync();
        await c.DisposeAsync();
    }

    [Fact]
    public async Task Batch_slots_are_capped_below_total()
    {
        var gate = new ConnectionGate(4, 2, 100);
        var held = new List<IAsyncDisposable>();
        for (int i = 0; i < 2; i++) held.Add(await gate.AcquireAsync(QueryClass.Batch));
        await Assert.ThrowsAsync<ConnPoolException>(() => gate.AcquireAsync(QueryClass.Batch));
        // Interactive still has reserved capacity.
        var interactive = await gate.AcquireAsync(QueryClass.Interactive);
        held.Add(interactive);
        foreach (var h in held) await h.DisposeAsync();
    }

    [Fact]
    public async Task InUse_tracks_active_leases()
    {
        var gate = new ConnectionGate(3, 1, 100);
        Assert.Equal(0, gate.InUse);
        var a = await gate.AcquireAsync(QueryClass.Interactive);
        Assert.Equal(1, gate.InUse);
        await a.DisposeAsync();
        Assert.Equal(0, gate.InUse);
    }
}
