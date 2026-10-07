# install.ps1 installs memstate on Windows: the memstated daemon and the
# memstate-mcp MCP server. It can also connect the server to your MCP
# clients and install the Claude Code skill and hooks.
#
# Usage, in PowerShell:
#   irm https://raw.githubusercontent.com/map588/memstate/main/install.ps1 | iex
#
# Requirements: Windows 10 1803 or later (for tar.exe) and Node 18 or later.
# The Claude Code skill also needs Python 3. The hooks also need Git Bash:
# Claude Code runs hook commands with Git Bash when it is present, and the
# hooks are bash scripts.
#
# Environment:
#   MEMSTATE_VERSION        release tag to install, for example v0.7.8 (default: latest)
#   MEMSTATE_SETUP          1 runs `memstate-mcp setup`, 0 skips it
#                           (default: run it in an interactive session)
#   MEMSTATE_INSTALL_SKILL  1 installs the Claude Code skill and hooks, 0 skips
#                           them (default: ask in an interactive session)
#   MEMSTATE_DOWNLOAD_URL   http(s) directory that holds the release assets;
#                           for tests of a local `make release`
#
# The programs go to %LOCALAPPDATA%\Programs\memstate, in the same layout
# as the repository, so the MCP proxy finds the daemon next to it:
#   server\memstated.exe    the daemon
#   server\memstate.exe     a copy that runs as the human CLI
#   server\memstate-mcp.cmd runs the MCP proxy
#   client\                 the MCP proxy and its node_modules
#   skill\ skill-precompact\ hooks\ configure-claude-hook.py   the Claude Code skills
# The script adds server\ to the user PATH. Run it again to update both parts.

# The script block gives the script its own scope, so `irm | iex` does not
# change the preferences or the functions of the session.
& {
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = 'Stop'
    # Windows PowerShell 5.1 draws a progress bar that makes downloads slow.
    $ProgressPreference = 'SilentlyContinue'
    # Windows PowerShell 5.1 does not always offer TLS 1.2, which GitHub needs.
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $Repo = 'map588/memstate'
    $Asset = 'memstated-windows-amd64.exe'
    $Bundle = 'memstate-mcp.tar.gz'
    $Root = Join-Path $env:LOCALAPPDATA 'Programs\memstate'
    $ServerDir = Join-Path $Root 'server'
    # Python and Node find the home directory through USERPROFILE, so the
    # script uses the same variable.
    $ClaudeHome = Join-Path $env:USERPROFILE '.claude'

    function Say([string]$Message) { Write-Host "install.ps1: $Message" }
    function Fail([string]$Message) { throw "install.ps1: error: $Message" }

    function Get-Choice([string]$Name) {
        $value = [Environment]::GetEnvironmentVariable($Name)
        if ($value -and $value -ne '0' -and $value -ne '1') {
            Fail "$Name must be 0 or 1, not '$value'"
        }
        return $value
    }

    function Test-Interactive {
        return [Environment]::UserInteractive -and -not [Console]::IsInputRedirected
    }

    # Invoke-Captured runs a program with an empty stdin and returns its exit
    # code and its stdout and stderr as text. PowerShell 5.1 turns the
    # stderr lines of a redirected program into errors, so .NET reads them.
    function Invoke-Captured([string]$File, [string[]]$Arguments, [hashtable]$Set = @{}, [string[]]$Unset = @()) {
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $File
        $psi.Arguments = ($Arguments | ForEach-Object { '"' + $_ + '"' }) -join ' '
        $psi.UseShellExecute = $false
        $psi.RedirectStandardInput = $true
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        foreach ($name in $Unset) { $psi.EnvironmentVariables.Remove($name) }
        foreach ($name in $Set.Keys) { $psi.EnvironmentVariables[$name] = $Set[$name] }
        $process = [System.Diagnostics.Process]::Start($psi)
        $process.StandardInput.Close()
        $stderr = $process.StandardError.ReadToEndAsync()
        $stdout = $process.StandardOutput.ReadToEnd()
        $process.WaitForExit()
        return [pscustomobject]@{ Code = $process.ExitCode; Output = $stdout + $stderr.Result }
    }

    function Get-File([string]$Url, [string]$Path) {
        Say "downloading $Url"
        Invoke-WebRequest -Uri $Url -OutFile $Path -UseBasicParsing
    }

    # Replace-File moves Source to Target. Windows does not let a program
    # overwrite or delete a running .exe, but it lets it rename one. The old
    # file goes aside first, the same as replaceBinary in server/upgrade.go.
    function Replace-File([string]$Source, [string]$Target) {
        $old = "$Target.old"
        if (Test-Path -LiteralPath $Target) {
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            Move-Item -LiteralPath $Target -Destination $old -Force
        }
        Move-Item -LiteralPath $Source -Destination $Target
        Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
    }

    # Find-Python returns the command for Python 3 as an array: the program
    # and its first arguments. The python.exe that the Microsoft Store puts
    # on PATH fails `--version` until Python is installed.
    function Find-Python {
        foreach ($candidate in @(, @('python')) + @(, @('py', '-3'))) {
            $command = Get-Command $candidate[0] -CommandType Application -ErrorAction SilentlyContinue |
                Select-Object -First 1
            if (-not $command) { continue }
            $run = @($command.Source) + @($candidate | Select-Object -Skip 1)
            $result = Invoke-Captured $run[0] (@($run | Select-Object -Skip 1) + '--version')
            if ($result.Code -eq 0 -and $result.Output -match 'Python 3') { return , $run }
        }
        return $null
    }

    # Find-GitBash returns the bash.exe of Git for Windows, which Claude Code
    # uses for hook commands. bash.exe in System32 is WSL, so the search
    # starts from git.exe.
    function Find-GitBash {
        if ($env:CLAUDE_CODE_GIT_BASH_PATH -and (Test-Path -LiteralPath $env:CLAUDE_CODE_GIT_BASH_PATH)) {
            return $env:CLAUDE_CODE_GIT_BASH_PATH
        }
        $candidates = @()
        $git = Get-Command git -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($git) {
            # git.exe is in <Git>\cmd, <Git>\bin or <Git>\mingw64\bin.
            $dir = Split-Path -Parent $git.Source
            $candidates += (Join-Path $dir 'bash.exe'), (Join-Path $dir '..\bin\bash.exe'), (Join-Path $dir '..\..\bin\bash.exe')
        }
        if ($env:ProgramFiles) { $candidates += Join-Path $env:ProgramFiles 'Git\bin\bash.exe' }
        foreach ($candidate in $candidates) {
            if (Test-Path -LiteralPath $candidate) { return (Resolve-Path -LiteralPath $candidate).Path }
        }
        return $null
    }

    function Install-Skill {
        $python = Find-Python
        if (-not $python) {
            Say 'warning: Python 3 not found. The skill scripts and the hook setup need it.'
            Say 'skipped the Claude Code skill.'
            return
        }
        $skillDir = Join-Path $ClaudeHome 'skills\memstate'
        $precompactDir = Join-Path $ClaudeHome 'skills\memstate-precompact'
        New-Item -ItemType Directory -Force -Path (Join-Path $ClaudeHome 'skills') | Out-Null
        if (Test-Path -LiteralPath $skillDir) { Remove-Item -LiteralPath $skillDir -Recurse -Force }
        if (Test-Path -LiteralPath $precompactDir) { Remove-Item -LiteralPath $precompactDir -Recurse -Force }
        Copy-Item -LiteralPath (Join-Path $Root 'skill') -Destination $skillDir -Recurse
        Copy-Item -LiteralPath (Join-Path $Root 'skill-precompact') -Destination $precompactDir -Recurse
        Say "installed the skills in $skillDir and $precompactDir"

        $bash = Find-GitBash
        if (-not $bash) {
            Say 'warning: Git Bash not found. Claude Code runs hooks with Git Bash, and the hooks are bash scripts.'
            Say 'skipped the hooks. Install Git for Windows, then run this script again.'
            return
        }
        $hooksDir = Join-Path $ClaudeHome 'hooks'
        New-Item -ItemType Directory -Force -Path $hooksDir | Out-Null
        $hookPaths = @()
        foreach ($hook in @('memstate-persist-reminder.sh', 'memstate-recall.sh')) {
            Copy-Item -LiteralPath (Join-Path $Root "hooks\$hook") -Destination (Join-Path $hooksDir $hook) -Force
            # Git Bash reads a backslash as an escape character, so the hook
            # command uses forward slashes.
            $hookPaths += ((Join-Path $hooksDir $hook) -replace '\\', '/')
        }
        $arguments = @($python | Select-Object -Skip 1) + (Join-Path $Root 'configure-claude-hook.py') + 'install' + $hookPaths
        $result = Invoke-Captured $python[0] $arguments
        if ($result.Code -ne 0) { Fail "the hook setup failed: $($result.Output)" }
        Say "installed the hooks in $hooksDir and added them to $(Join-Path $ClaudeHome 'settings.json')"
        Say "the recall hook needs a shared daemon: set MEMSTATE_ADDR, or start 'memstated --addr 127.0.0.1:8765'"
    }

    $setupChoice = Get-Choice 'MEMSTATE_SETUP'
    $skillChoice = Get-Choice 'MEMSTATE_INSTALL_SKILL'

    # Windows 11 on ARM64 runs the x64 build.
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
    if ($arch -ne 'AMD64' -and $arch -ne 'ARM64') { Fail "unsupported architecture: $arch" }

    # Use the tar.exe of Windows. The GNU tar of Git reads C: as a host name.
    $tar = Join-Path $env:SystemRoot 'System32\tar.exe'
    if (-not (Test-Path -LiteralPath $tar)) { Fail 'tar.exe not found. Windows 10 1803 or later is required.' }

    # The MCP proxy is a Node program. Check for Node before any download.
    $nodeCommand = Get-Command node -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $nodeCommand) {
        Fail 'Node 18 or later is required for the MCP server. Install it from https://nodejs.org, then run this script again.'
    }
    $node = $nodeCommand.Source
    $nodeVersion = (Invoke-Captured $node @('-p', 'process.versions.node')).Output.Trim()
    if ([int]($nodeVersion.Split('.')[0]) -lt 18) {
        Fail "Node 18 or later is required for the MCP server; found v$nodeVersion"
    }

    # Find the tag once, so both assets come from the same release.
    if ($env:MEMSTATE_DOWNLOAD_URL) {
        $base = $env:MEMSTATE_DOWNLOAD_URL.TrimEnd('/')
        $tag = "from $base"
    } else {
        $tag = $env:MEMSTATE_VERSION
        if (-not $tag) { $tag = 'latest' }
        if ($tag -ne 'latest' -and -not $tag.StartsWith('v')) { $tag = "v$tag" }
        if ($tag -eq 'latest') {
            try {
                $tag = (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$Repo/releases/latest").tag_name
            } catch {
                Fail "cannot find the latest release: $($_.Exception.Message)"
            }
        }
        $base = "https://github.com/$Repo/releases/download/$tag"
    }
    Say "installing memstate $tag"

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('memstate-install-' + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $daemonFile = Join-Path $tmp 'memstated.exe'
        $bundleFile = Join-Path $tmp $Bundle
        try { Get-File "$base/$Asset" $daemonFile } catch { Fail "download failed: $base/$Asset" }
        try { Get-File "$base/$Bundle" $bundleFile } catch {
            Fail "download failed: $base/$Bundle (releases before v0.7.8 do not have it)"
        }
        if ((Invoke-Captured $daemonFile @('--help')).Code -ne 0) { Fail 'the downloaded memstated does not run' }
        $unpacked = Join-Path $tmp 'bundle'
        New-Item -ItemType Directory -Path $unpacked | Out-Null
        & $tar -xzf $bundleFile -C $unpacked
        if ($LASTEXITCODE -ne 0) { Fail "cannot unpack $Bundle" }
        if (-not (Test-Path -LiteralPath (Join-Path $unpacked 'client\dist\index.js'))) {
            Fail "$Bundle has no client\dist\index.js"
        }

        New-Item -ItemType Directory -Force -Path $ServerDir | Out-Null
        foreach ($entry in Get-ChildItem -LiteralPath $unpacked) {
            $target = Join-Path $Root $entry.Name
            if (Test-Path -LiteralPath $target) {
                try { Remove-Item -LiteralPath $target -Recurse -Force } catch {
                    Fail "cannot replace $target. Close the programs that use memstate (your MCP clients), then run this script again."
                }
            }
            Move-Item -LiteralPath $entry.FullName -Destination $target
        }
        # memstated dispatches on the name it runs as: as memstate it is the CLI.
        $cliCopy = Join-Path $tmp 'memstate.exe'
        Copy-Item -LiteralPath $daemonFile -Destination $cliCopy
        Replace-File $daemonFile (Join-Path $ServerDir 'memstated.exe')
        Replace-File $cliCopy (Join-Path $ServerDir 'memstate.exe')
        Set-Content -LiteralPath (Join-Path $ServerDir 'memstate-mcp.cmd') -Encoding Ascii `
            -Value '@node "%~dp0..\client\dist\index.js" %*'
        Say "installed $Root"

        # Add server\ to the user PATH. The registry value is read and written
        # unexpanded, so entries such as %USERPROFILE%\bin stay as they are.
        $environment = Get-Item -Path 'HKCU:\Environment'
        $userPath = $environment.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
        $entries = @($userPath -split ';' | Where-Object { $_ })
        if ($entries -notcontains $ServerDir) {
            Set-ItemProperty -Path 'HKCU:\Environment' -Name 'Path' -Type ExpandString `
                -Value (($entries + $ServerDir) -join ';')
            # SetEnvironmentVariable tells the running programs, for example
            # Explorer, that the environment changed.
            [Environment]::SetEnvironmentVariable('MEMSTATE_INSTALL_PATH_CHANGED', '1', 'User')
            [Environment]::SetEnvironmentVariable('MEMSTATE_INSTALL_PATH_CHANGED', $null, 'User')
            Say "added $ServerDir to your PATH. Open a new terminal to use it."
        }
        if (@($env:Path -split ';') -notcontains $ServerDir) { $env:Path = "$ServerDir;$env:Path" }

        # The proxy starts its own daemon on a scratch DB. The check never
        # uses the DB, the log or the shared daemon of the user.
        $mcp = Join-Path $Root 'client\dist\index.js'
        $smoke = Join-Path $tmp 'smoke'
        New-Item -ItemType Directory -Path $smoke | Out-Null
        $check = Invoke-Captured $node @($mcp, '--test') `
            -Set @{ MEMSTATE_DB = (Join-Path $smoke 'memstate.db'); MEMSTATE_NO_UPDATE_CHECK = '1' } `
            -Unset @('MEMSTATE_ADDR', 'MEMSTATE_BIN')
        if ($check.Code -ne 0) {
            Write-Host $check.Output
            Fail 'memstate-mcp did not start the daemon'
        }
        $version = ''
        if ($check.Output -match '"version":"([^"]+)"') { $version = "v$($Matches[1])" }
        Say "check passed: memstate-mcp started memstated $version"

        # Another copy on PATH, for example from `make install`. The system
        # PATH comes before the user PATH, so that copy can win in a new
        # terminal.
        foreach ($name in @('memstated', 'memstate', 'memstate-mcp')) {
            foreach ($found in @(Get-Command $name -All -ErrorAction SilentlyContinue)) {
                if ((Split-Path -Parent $found.Source) -ne $ServerDir) {
                    Say "warning: another '$name' is on PATH at $($found.Source). Remove it, so the new copy runs."
                }
            }
        }

        # setup finds the MCP clients, asks for the embed model, and asks
        # before it writes a config.
        if (-not $setupChoice) {
            $setupChoice = '0'
            if (Test-Interactive) { $setupChoice = '1' }
        }
        if ($setupChoice -eq '1') {
            if (Test-Interactive) {
                & $node $mcp setup
                $setupCode = $LASTEXITCODE
            } else {
                $setup = Invoke-Captured $node @($mcp, 'setup')
                Write-Host $setup.Output
                $setupCode = $setup.Code
            }
            if ($setupCode -ne 0) { Say 'warning: memstate-mcp setup failed. Run it again later: memstate-mcp setup' }
        } else {
            Say 'to connect memstate to your MCP clients, run: memstate-mcp setup'
        }

        if ($skillChoice -eq '1') {
            Install-Skill
        } elseif (-not $skillChoice) {
            $install = $false
            if (Test-Interactive) {
                $hint = '[y/N]'
                if (Test-Path -LiteralPath $ClaudeHome) { $hint = '[Y/n]' }
                $answer = Read-Host "Install the Claude Code skill and hooks? $hint"
                if ($answer -match '^[Yy]') { $install = $true }
                elseif (-not $answer -and $hint -eq '[Y/n]') { $install = $true }
            }
            if ($install) {
                Install-Skill
            } else {
                Say 'skipped the Claude Code skill. To install it later, set MEMSTATE_INSTALL_SKILL=1 and run this script again.'
            }
        }

        Say 'done. Later, run this script again to update memstated and memstate-mcp together.'
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
