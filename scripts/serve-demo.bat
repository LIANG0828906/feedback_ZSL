@echo off
setlocal
set CGO_ENABLED=0
go run .\cmd\maskpipe serve -sample samples\local-seed -scene blocks -db reports\web-diagnostic.db -addr 127.0.0.1:8088
