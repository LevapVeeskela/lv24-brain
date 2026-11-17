@echo off
rem Daily company brain refresh: engine update, code graphs, GitHub/GitLab/mail
rem connectors, data sync and shared agent instructions.
rem Task: "Company Brain Graph Refresh". Config comes from `brain config use`
rem or BRAIN_CONFIG; keep this file ASCII and CRLF (cmd.exe).
setlocal
set BRAIN="C:\0project\lv24-brain\bin\brain.exe"
set LOG="%LOCALAPPDATA%\lv24-brain\graphs-task.log"
if not exist "%LOCALAPPDATA%\lv24-brain" mkdir "%LOCALAPPDATA%\lv24-brain"
rem Keep the log small: start over once it passes ~500 KB.
for %%F in (%LOG%) do if %%~zF GTR 500000 del %LOG%
echo == %DATE% %TIME% == >> %LOG%
set RC=0
rem Apply a newer signed release first (keeps the previous binary for rollback).
%BRAIN% update --yes >> %LOG% 2>&1 || set RC=1
%BRAIN% graphs --changed-only >> %LOG% 2>&1 || set RC=1
%BRAIN% connector run --all --days 7 >> %LOG% 2>&1 || set RC=1
rem Commit and push the refreshed private data so the other machines see it.
%BRAIN% sync --push >> %LOG% 2>&1 || set RC=1
rem Refresh the shared agent instructions from lv24-brain-data/global/AGENTS.md.
%BRAIN% agents sync >> %LOG% 2>&1 || set RC=1
echo == exit %RC% == >> %LOG%
exit /b %RC%
