@echo off
setlocal
set CGO_ENABLED=0

echo [1/6] Building maskpipe.exe...
go build -o bin\maskpipe.exe .\cmd\maskpipe
if errorlevel 1 exit /b 1

echo [2/6] Generating deterministic local samples...
bin\maskpipe.exe generate -root samples\local-seed -seed 424242
if errorlevel 1 exit /b 1

echo [3/6] Validating manifest, PNG and truth fingerprint...
bin\maskpipe.exe validate -sample samples\local-seed -scene blocks
if errorlevel 1 exit /b 1

echo [4/6] Running parameter example with coarse preview...
bin\maskpipe.exe run -sample samples\local-seed -scene blocks -params configs\params.example.json -db reports\example-blocks.db -out reports\example-blocks.json -mode full -preview
if errorlevel 1 exit /b 1

echo [5/6] Running live/resume/full pixel consistency...
bin\maskpipe.exe threeway -sample samples\local-seed -scene blocks -out reports\threeway-blocks
if errorlevel 1 exit /b 1

echo [6/6] Running independent SQLite verification...
bin\maskpipe.exe verify -db reports\threeway-blocks\dbs\full.db -manifest samples\local-seed\manifest.json -scene blocks -out reports\threeway-blocks\verify-cli.json
if errorlevel 1 exit /b 1

echo.
echo Demo passed. Start diagnostics with scripts\serve-demo.bat
