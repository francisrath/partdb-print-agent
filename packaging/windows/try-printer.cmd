@echo off
rem Runs the guided printer test, then waits so the window stays open.
cd /d "%~dp0"
if not exist pdb-print.exe (
  echo pdb-print.exe is not next to this file. Unzip the whole folder first, then run try-printer.cmd from there.
  pause
  exit /b 1
)
pdb-print.exe try
pause
