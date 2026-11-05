package update

// swapScript is run by cmd.exe after the updating process exits. Arguments are
// passed separately so no path is interpolated into the script text.
const swapScript = `@echo off
setlocal
set "TARGET=%~1"
set "SOURCE=%~2"
set /a tries=0
:retry
if not exist "%SOURCE%" goto done
move /Y "%SOURCE%" "%TARGET%" >nul 2>&1
if not errorlevel 1 goto done
set /a tries+=1
if %tries% GEQ 120 goto done
timeout /t 1 /nobreak >nul 2>&1 <nul
goto retry
:done
del "%~f0" >nul 2>&1
`
