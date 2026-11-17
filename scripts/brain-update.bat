@echo off
rem Frequent engine update and git auto-sync for this machine. Task:
rem "Company Brain Update", every 30 minutes. Applies a newer signed release
rem right after it is published and pushes brain data changes; the previous
rem binary is kept for `brain update --rollback`.
rem Keep this file ASCII and CRLF (cmd.exe).
setlocal
set BRAIN="C:\0project\lv24-brain\bin\brain.exe"
set LOG="%LOCALAPPDATA%\lv24-brain\update-task.log"
if not exist "%LOCALAPPDATA%\lv24-brain" mkdir "%LOCALAPPDATA%\lv24-brain"
rem Keep the log small: start over once it passes ~200 KB.
for %%F in (%LOG%) do if %%~zF GTR 200000 del %LOG%
echo == %DATE% %TIME% == >> %LOG%
%BRAIN% update --yes >> %LOG% 2>&1
set RC=%ERRORLEVEL%
rem Auto-sync: commit, rebase and push; a clean tree is a cheap no-op. An
rem interrupted rebase is recovered by the same command before it pulls.
%BRAIN% sync --push >> %LOG% 2>&1
if errorlevel 1 set RC=1
echo == exit %RC% == >> %LOG%
exit /b %RC%
