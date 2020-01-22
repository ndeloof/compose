# Builds the Windows binary.
#
# From a fresh 64-bit Windows 10 install, prepare the system as follows:
#
# 1. Install Git:
#
#        http://git-scm.com/download/win
#
# 2. Clone the repository:
#
#        $ git clone https://github.com/docker/compose.git
#        $ cd compose
#
# 3. Build the binary:
#
#        .\script\build\windows.ps1

$ErrorActionPreference = "Stop"

Invoke-Expression -Command "python --version" | Tee-Object -Variable python_version
if ($python_version eq "Python 3.7.6") {
    Invoke-WebRequest -Uri "https://www.python.org/ftp/python/3.7.6/python-3.7.6-embed-amd64.zip" -OutFile "C:\temp\python.zip"
    Expand-Archive -LiteralPath C:\temp\python.zip -DestinationPath C:\temp\python InvoicesUnzipped
    Set-Item -Path Env:Path -Value ("C:\temp\python;" + $Env:Path)
}

pip install 'virtualenv==16.7.9'
Set-ExecutionPolicy -Scope CurrentUser RemoteSigned

# Remove virtualenv
if (Test-Path venv) {
    Remove-Item -Recurse -Force .\venv
}

# Remove .pyc files
Get-ChildItem -Recurse -Include *.pyc | foreach ($_) { Remove-Item $_.FullName }

# Create virtualenv
virtualenv .\venv

# pip and pyinstaller generate lots of warnings, so we need to ignore them
$ErrorActionPreference = "Continue"

.\venv\Scripts\pip install pypiwin32==223
.\venv\Scripts\pip install -r requirements.txt
.\venv\Scripts\pip install --no-deps .
.\venv\Scripts\pip install -r requirements-build.txt

git rev-parse --short HEAD | out-file -encoding ASCII compose\GITSHA

# Build binary
.\venv\Scripts\pyinstaller .\docker-compose.spec
$ErrorActionPreference = "Stop"

Move-Item -Force .\dist\docker-compose.exe .\dist\docker-compose-Windows-x86_64.exe
.\dist\docker-compose-Windows-x86_64.exe --version
