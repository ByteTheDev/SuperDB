@echo off
REM install.bat — build + install SuperDB (Windows, MSYS2 ucrt64 gcc).
REM Usage: tools\install.bat [--dir %LocalAppData%\SuperDB] [--no-assoc] [--no-vscode]
setlocal

set DEST=%LocalAppData%\SuperDB
set ASSOC=1
set VSCODE=1

:args
if "%~1"=="" goto done
if "%~1"=="--dir" set DEST=%~2 & shift & shift & goto args
if "%~1"=="--no-assoc" set ASSOC=0 & shift & goto args
if "%~1"=="--no-vscode" set VSCODE=0 & shift & goto args
echo unknown flag: %~1 & exit /b 2
:done

cd /d "%~dp0.."

echo ==^> building
call build.bat
if errorlevel 1 exit /b 1

echo ==^> installing to %DEST%
mkdir "%DEST%\bin" 2>nul
copy /y bin\superdb-server.exe "%DEST%\bin\" >nul
copy /y bin\superdb-cli.exe "%DEST%\bin\" >nul
copy /y bin\superdb.exe "%DEST%\bin\" >nul

REM persist PATH for future shells
for /f "tokens=2*" %%a in ('reg query HKCU\Environment /v Path 2^>nul') do set UPATH=%%b
echo %UPATH% | findstr /c:"%DEST%\bin" >nul
if errorlevel 1 setx Path "%UPATH%;%DEST%\bin" >nul && echo added to user PATH (reopen terminal to use)

if "%ASSOC%"=="1" (
  echo ==^> associating .sdb
  "%DEST%\bin\superdb.exe" assoc
)

if "%VSCODE%"=="1" (
  echo ==^> installing VS Code extension
  mkdir "%USERPROFILE%\.vscode\extensions\superdb.vscode-superdb-0.1.0" 2>nul
  xcopy /e /i /y editors\vscode-superdb "%USERPROFILE%\.vscode\extensions\superdb.vscode-superdb-0.1.0" >nul
  echo reload VS Code to activate.
)

echo install OK — try: superdb doctor
