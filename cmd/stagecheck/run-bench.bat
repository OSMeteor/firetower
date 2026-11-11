@echo off
setlocal
rem Firetower single-machine bench: Windows first, then the same bench in WSL if WSL is installed.
rem Results go to the logs folder. Takes about 15-25 minutes. Keep this window open.
set HERE=%~dp0
set LOGS=%HERE%logs
if exist "%LOGS%" rmdir /s /q "%LOGS%"
mkdir "%LOGS%"
set WORK=%TEMP%\ftbench
if exist "%WORK%" rmdir /s /q "%WORK%"
mkdir "%WORK%"

echo [1/3] machine info ...
powershell -NoProfile -Command "Get-CimInstance Win32_Processor | Select-Object Name,NumberOfCores,NumberOfLogicalProcessors | Format-List; Get-CimInstance Win32_ComputerSystem | Select-Object @{n='MemoryGB';e={[math]::Round($_.TotalPhysicalMemory/1GB,1)}} | Format-List; [Environment]::OSVersion.VersionString" > "%LOGS%\00-machine.log" 2>&1

echo [2/3] bench on Windows ...
cd /d "%WORK%"
"%HERE%bench-windows.exe" -stage bench > "%LOGS%\01-bench-windows.log" 2>&1
>> "%LOGS%\01-bench-windows.log" echo exit=%ERRORLEVEL%

echo [3/3] bench in WSL ...
where wsl >nul 2>&1
if errorlevel 1 (
  echo no wsl > "%LOGS%\02-bench-wsl.log"
  goto done
)
wsl -e sh -c "nproc; free -m; uname -r" > "%LOGS%\02-wsl-machine.log" 2>&1
wsl -e sh -c "ulimit -n 65535 2>/dev/null || ulimit -n $(ulimit -Hn); cp $(wslpath '%HERE%bench-linux') /tmp/ftbench && chmod +x /tmp/ftbench && cd /tmp && /tmp/ftbench -stage bench" > "%LOGS%\02-bench-wsl.log" 2>&1
>> "%LOGS%\02-bench-wsl.log" echo exit=%ERRORLEVEL%

:done
> "%LOGS%\99-finished.log" echo finished
echo finished. results are in %LOGS%
