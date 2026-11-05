@echo off
rem LV24 Brain Windows installer wrapper. All arguments are passed through.
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
exit /b %ERRORLEVEL%
