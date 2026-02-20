@echo off
setlocal

set INSTALL_DIR=%USERPROFILE%\.local\bin

:: Check for Go
where go >nul 2>&1
if %errorlevel% neq 0 (
    echo Error: Go is not installed.
    echo Install it from https://go.dev/dl/
    exit /b 1
)

echo Building larva...
cd /d "%~dp0"
go get github.com/BurntSushi/toml
go build -o larva.exe larva.go
if %errorlevel% neq 0 (
    echo Error: Build failed.
    exit /b 1
)

echo Installing to %INSTALL_DIR%...
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"
move /y larva.exe "%INSTALL_DIR%\larva.exe" >nul

:: Check if install dir is on PATH
echo %PATH% | findstr /i "%INSTALL_DIR%" >nul 2>&1
if %errorlevel% neq 0 (
    echo.
    echo Add %INSTALL_DIR% to your PATH:
    echo   Settings ^> System ^> Environment Variables ^> Path ^> New
    echo   %INSTALL_DIR%
)

echo Done. Run 'larva' from any project with a larva.toml.
