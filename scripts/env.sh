# Source this file to load the user-local .NET 8 SDK and ICU runtime.
# No root/package installation is required beyond scripts/install-dotnet.sh
# and scripts/install-icu.sh.

export DOTNET_HOME="${DOTNET_HOME:-$HOME/.dotnet8}"
export ICU_HOME="${ICU_HOME:-$HOME/.local/icu/extracted/usr/lib/x86_64-linux-gnu}"

case ":$PATH:" in
    *":$DOTNET_HOME:"*) ;;
    *) export PATH="$DOTNET_HOME:$PATH" ;;
esac

if [ -d "$ICU_HOME" ]; then
  case ":${LD_LIBRARY_PATH:-}:" in
      *":$ICU_HOME:"*) ;;
      *) export LD_LIBRARY_PATH="$ICU_HOME${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ;;
  esac
fi

unset DOTNET_SYSTEM_GLOBALIZATION_INVARIANT
export DOTNET_CLI_TELEMETRY_OPTOUT=1
export DOTNET_NOLOGO=1
export DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1
export DOTNET_CLI_USE_MSBUILD_SERVER=0
export DOTNET_PROCESSOR_COUNT="${DOTNET_PROCESSOR_COUNT:-2}"
export DOTNET_gcServer=0
