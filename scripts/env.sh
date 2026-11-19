# Shared environment for OrderRouter runs. Resource caps: 2 CPUs, ~1.25GiB.
export DOTNET_ROOT="${DOTNET_ROOT:-/home/liang/.dotnet}"
export PATH="$DOTNET_ROOT:$PATH"
export DOTNET_CLI_TELEMETRY_OPTOUT=1
export DOTNET_NOLOGO=1
# libicu is provided by the shared toolchain bundle.
export LD_LIBRARY_PATH="/tmp/pgroot/usr/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH:-}"
# Keep the managed heap well inside the 1.25GiB budget (shared with SQL Server).
export DOTNET_GCHeapHardLimit="${DOTNET_GCHeapHardLimit:-0x18000000}"
export DOTNET_GCConserveMemory=8
