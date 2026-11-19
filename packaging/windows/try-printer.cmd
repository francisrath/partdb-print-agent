@echo off
rem Runs the guided printer test, then waits so the window stays open.
cd /d "%~dp0"
pdb-print.exe try
pause
