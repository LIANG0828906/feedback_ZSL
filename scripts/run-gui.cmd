@echo off
setlocal
cd /d "%~dp0\.."
dotnet run --project src\TacticalField.WinForms\TacticalField.WinForms.csproj -c Release
endlocal

