param(
    [string]$ProjectName = "mcp-postgres",
    [string]$ImageName = "mcp-postgres",
    [int]$HostPort = 8080,
    [string]$DatabaseUrl = "",
    [string]$McpApiKey = "",
    [string]$ImageTag = "latest",
    [string]$ComposeFile = "",
    [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"
$ProjectRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
if ([string]::IsNullOrWhiteSpace($ComposeFile)) {
    $ComposeFile = Join-Path $ProjectRoot "Docker\docker-compose.yaml"
}
if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
    $ConfigPath = "/app/Docker/config.docker.yaml"
}
$Dockerfile = Join-Path $ProjectRoot "Docker\Dockerfile"
$EnvFile = Join-Path $ProjectRoot ".env"
$EnvExampleFile = Join-Path $ProjectRoot ".env.example"
$LastComposeSucceeded = $false
$DatabaseUrlWasProvided = -not [string]::IsNullOrWhiteSpace($DatabaseUrl)
$McpApiKeyWasProvided = -not [string]::IsNullOrWhiteSpace($McpApiKey)

function Write-Title {
    param([string]$Text)
    Write-Host ""
    Write-Host "=== $Text ===" -ForegroundColor Cyan
}

function Write-FelsenBanner {
    Write-Host "  ______    _                  _____         _                 _             _           " -ForegroundColor Cyan
    Write-Host " |  ____|  | |                |_   _|       | |               | |           (_)          " -ForegroundColor Cyan
    Write-Host " | |__ ___ | |___  ___ _ __     | | ___  ___| |__  _ __   ___ | | ___   __ _ _  ___  ___ " -ForegroundColor Cyan
    Write-Host " |  __/ _ \| / __|/ _ \ '_ \    | |/ _ \/ __| '_ \| '_ \ / _ \| |/ _ \ / _` | |/ _ \/ __|" -ForegroundColor Cyan
    Write-Host " | | |  __/| \__ \  __/ | | |   | |  __/ (__| | | | | | | (_) | | (_) | (_| | |  __/\__ \" -ForegroundColor Cyan
    Write-Host " |_|  \___||_|___/\___|_| |_|   \_/\___|\___|_| |_|_| |_|\___/|_|\___/ \__, |_|\___||___/" -ForegroundColor Cyan
    Write-Host "                                                                        __/ |             " -ForegroundColor Cyan
    Write-Host "                                                                       |___/              " -ForegroundColor Cyan
}

function Test-Docker {
    try {
        & docker version | Out-Null
        if ($LASTEXITCODE -ne 0) {
            Write-Host "Docker nao esta respondendo. Abra o Docker Desktop e tente novamente." -ForegroundColor Red
            return $false
        }
        return $true
    }
    catch {
        Write-Host "Docker nao esta disponivel ou sem permissao para este usuario." -ForegroundColor Red
        Write-Host $_.Exception.Message -ForegroundColor DarkGray
        Write-Host "Abra o Docker Desktop, verifique se ele terminou de iniciar e confirme as permissoes do usuario." -ForegroundColor Yellow
        return $false
    }
}

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Test-IsWindowsHost {
    if (Get-Variable -Name IsWindows -Scope Global -ErrorAction SilentlyContinue) {
        return $IsWindows
    }
    return [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
}

function Initialize-EnvFile {
    if (Test-Path -LiteralPath $EnvFile) {
        return
    }

    Write-Title "Criar .env"
    if (Test-Path -LiteralPath $EnvExampleFile) {
        Copy-Item -LiteralPath $EnvExampleFile -Destination $EnvFile
        Write-Host ".env criado a partir de .env.example" -ForegroundColor Green
        return
    }

    Set-Content -LiteralPath $EnvFile -Value "" -NoNewline
    Write-Host ".env criado vazio" -ForegroundColor Yellow
}

function New-DevSecret {
    param([int]$Length = 32)

    $buffer = [byte[]]::new($Length)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($buffer)
    }
    finally {
        $rng.Dispose()
    }
    return [Convert]::ToBase64String($buffer).TrimEnd("=").Replace("+", "-").Replace("/", "_")
}

function Read-EnvValues {
    $values = @{}
    if (-not (Test-Path -LiteralPath $EnvFile)) {
        return $values
    }

    foreach ($line in Get-Content -LiteralPath $EnvFile) {
        if ($line -match "^\s*#" -or $line -notmatch "=") {
            continue
        }
        $parts = $line -split "=", 2
        $values[$parts[0].Trim()] = $parts[1]
    }
    return $values
}

function Sync-DevEnv {
    Initialize-EnvFile
    $values = Read-EnvValues

    if (-not $PSBoundParameters.ContainsKey("HostPort") -and $values.ContainsKey("HTTP_PORT") -and $values["HTTP_PORT"] -match "^\d+$") {
        $script:HostPort = [int]$values["HTTP_PORT"]
    }

    if (-not $DatabaseUrlWasProvided) {
        if ($values.ContainsKey("DATABASE_URL") -and -not [string]::IsNullOrWhiteSpace($values["DATABASE_URL"])) {
            $script:DatabaseUrl = $values["DATABASE_URL"]
        }
    }

    if (-not $McpApiKeyWasProvided) {
        if ($values.ContainsKey("MCP_API_KEY") -and -not [string]::IsNullOrWhiteSpace($values["MCP_API_KEY"])) {
            $script:McpApiKey = $values["MCP_API_KEY"]
        }
        else {
            $script:McpApiKey = New-DevSecret 32
        }
    }

    if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
        if ($values.ContainsKey("POSTGRES_MCP_CONFIG") -and -not [string]::IsNullOrWhiteSpace($values["POSTGRES_MCP_CONFIG"])) {
            $script:ConfigPath = $values["POSTGRES_MCP_CONFIG"]
        }
        else {
            $script:ConfigPath = "/app/Docker/config.docker.yaml"
        }
    }
}

function Save-EnvFile {
    Sync-Env

    $lines = @()
    $lines += "# Postgres MCP Server"
    $lines += "# Gerado automaticamente pelo script de dev"
    $lines += ""
    $lines += "POSTGRES_MCP_CONFIG=$ConfigPath"
    $lines += "DATABASE_URL=$DatabaseUrl"
    $lines += "MCP_API_KEY=$McpApiKey"
    $lines += "MCP_WRITER_API_KEY=$McpApiKey"
    $lines += "MCP_DDL_API_KEY=$McpApiKey"
    $lines += "POSTGRES_MCP_API_KEY=$McpApiKey"
    $lines += "HTTP_PORT=$HostPort"
    $lines += "MCP_PORT=$HostPort"
    $lines += "POSTGRES_MCP_PORT=$HostPort"
    $lines += "IMAGE_NAME=$ImageName"
    $lines += "HTTP_BEARER_TOKEN=$McpApiKey"
    $lines += "MCP_BEARER_TOKEN=$McpApiKey"
    $lines += "MCP_ALLOWED_ORIGINS="
    $lines += ""

    Set-Content -LiteralPath $EnvFile -Value ($lines -join "`n") -NoNewline
    Write-Host ".env atualizado" -ForegroundColor Green
}

function Sync-Env {
    Sync-DevEnv

    if ([string]::IsNullOrWhiteSpace($DatabaseUrl)) {
        $script:DatabaseUrl = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
    }
}

function Enable-WindowsFirewallPorts {
    Write-Title "Liberar portas no Firewall do Windows"
    Sync-DevEnv

    if (-not (Test-IsWindowsHost)) {
        Write-Host "Esta etapa e especifica do Windows." -ForegroundColor Yellow
        return
    }

    if (-not (Test-IsAdministrator)) {
        Write-Host "Execute este script como Administrador para criar regras no Firewall do Windows." -ForegroundColor Yellow
        Write-Host "Porta que precisa entrada TCP: $HostPort" -ForegroundColor Yellow
        return
    }

    $ruleName = "MCP Postgres Docker TCP $HostPort"
    $existingRule = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
    if ($existingRule) {
        Set-NetFirewallRule -DisplayName $ruleName -Enabled True -Profile Any -Direction Inbound -Action Allow | Out-Null
        Write-Host "Regra ja existente habilitada: TCP $HostPort" -ForegroundColor Green
        return
    }

    New-NetFirewallRule `
        -DisplayName $ruleName `
        -Direction Inbound `
        -Action Allow `
        -Protocol TCP `
        -LocalPort $HostPort `
        -Profile Any | Out-Null
    Write-Host "Regra criada: TCP $HostPort" -ForegroundColor Green
}

function Set-ComposeEnvironment {
    Sync-Env

    $env:COMPOSE_PROJECT_NAME = $ProjectName
    $env:IMAGE_NAME = $ImageName
    $env:HTTP_PORT = "$HostPort"
    $env:MCP_PORT = "$HostPort"
    $env:POSTGRES_MCP_PORT = "$HostPort"
    $env:POSTGRES_MCP_CONFIG = $ConfigPath
    $env:DATABASE_URL = $DatabaseUrl
    $env:MCP_API_KEY = $McpApiKey
    $env:MCP_WRITER_API_KEY = $McpApiKey
    $env:MCP_DDL_API_KEY = $McpApiKey
    $env:POSTGRES_MCP_API_KEY = $McpApiKey
    $env:HTTP_BEARER_TOKEN = $McpApiKey
    $env:MCP_BEARER_TOKEN = $McpApiKey
    $env:MCP_ALLOWED_ORIGINS = ""
}

function Invoke-Compose {
    param([string[]]$ArgsList)

    $script:LastComposeSucceeded = $false
    if (-not (Test-Docker)) { return }

    Initialize-EnvFile
    Set-ComposeEnvironment

    Push-Location $ProjectRoot
    try {
        & docker compose -f $ComposeFile @ArgsList
        if ($LASTEXITCODE -ne 0) {
            throw "docker compose falhou com codigo de saida $LASTEXITCODE"
        }
        $script:LastComposeSucceeded = $true
    }
    finally {
        Pop-Location
    }
}

function Invoke-ImageBuild {
    Write-Title "Build da imagem Docker"
    if (-not (Test-Docker)) { return }
    if (-not (Test-Path -LiteralPath $Dockerfile)) {
        throw "Dockerfile nao encontrado: $Dockerfile"
    }

    Push-Location $ProjectRoot
    try {
        & docker build -f $Dockerfile -t "$ImageName`:$ImageTag" .
        if ($LASTEXITCODE -ne 0) {
            throw "docker build falhou com codigo de saida $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
}

function Invoke-StackBuild {
    Write-Title "Build da stack"
    Invoke-Compose @("build")
}

function Start-Stack {
    Write-Title "Subir stack"
    Enable-WindowsFirewallPorts
    Invoke-Compose @("up", "-d", "--build")
    if ($script:LastComposeSucceeded) {
        if (Test-PostgresHealth) {
            Write-StackInfo
        }
        else {
            Write-Host "Stack iniciada mas Postgres nao esta pronto. Verifique os logs." -ForegroundColor Yellow
        }
    }
}

function Stop-Stack {
    Write-Title "Parar stack"
    Invoke-Compose @("down")
}

function Restart-Stack {
    Stop-Stack
    Start-Stack
}

function Recreate-Stack {
    Write-Title "Recriar stack"
    Enable-WindowsFirewallPorts
    Invoke-Compose @("up", "-d", "--build", "--force-recreate")
    if ($script:LastComposeSucceeded) {
        if (Test-PostgresHealth) {
            Write-StackInfo
        }
        else {
            Write-Host "Stack iniciada mas Postgres nao esta pronto. Verifique os logs." -ForegroundColor Yellow
        }
    }
}

function Remove-StackVolumes {
    Write-Title "Remover stack e volumes"
    Write-Host "Atencao: isso pode remover volumes de dados." -ForegroundColor Yellow
    $confirm = Read-Host "Digite REMOVER para confirmar"
    if ($confirm -ne "REMOVER") {
        Write-Host "Operacao cancelada." -ForegroundColor Yellow
        return
    }
    Invoke-Compose @("down", "-v")
}

function Show-Logs {
    Write-Title "Logs"
    Invoke-Compose @("logs", "-f")
}

function Show-Status {
    Write-Title "Status"
    Invoke-Compose @("ps")
}

function Show-ComposeConfig {
    Write-Title "Config Docker Compose"
    Invoke-Compose @("config")
}

function Test-PostgresHealth {
    Sync-Env
    $containerName = "${ProjectName}-postgres-1"
    
    Write-Host "Verificando Postgres..." -ForegroundColor Yellow
    
    $maxAttempts = 30
    $attempt = 0
    
    while ($attempt -lt $maxAttempts) {
        $status = docker inspect --format='{{.State.Health.Status}}' $containerName 2>$null
        if ($status -eq "healthy") {
            Write-Host "Postgres esta healthy!" -ForegroundColor Green
            return $true
        }
        
        $attempt++
        Write-Host "Aguardando Postgres... ($attempt/$maxAttempts)" -ForegroundColor Yellow
        Start-Sleep -Seconds 2
    }
    
    Write-Host "Postgres nao ficou healthy apos $($maxAttempts * 2) segundos" -ForegroundColor Red
    Write-Host "Verifique os logs com a opcao 8" -ForegroundColor Yellow
    return $false
}

function Test-StackHealth {
    Write-Title "Verificar saude da stack"
    
    if (-not (Test-Docker)) { return }
    
    Write-Host "Verificando containers..." -ForegroundColor Yellow
    
    $postgresStatus = docker inspect --format='{{.State.Status}}' "${ProjectName}-postgres-1" 2>$null
    $mcpStatus = docker inspect --format='{{.State.Status}}' "${ProjectName}-postgres-mcp-1" 2>$null
    
    Write-Host ""
    Write-Host "Postgres:     $(if ($postgresStatus -eq 'running') { 'running' } else { $postgresStatus })" -ForegroundColor $(if ($postgresStatus -eq 'running') { 'Green' } else { 'Red' })
    Write-Host "MCP Server:   $(if ($mcpStatus -eq 'running') { 'running' } else { $mcpStatus })" -ForegroundColor $(if ($mcpStatus -eq 'running') { 'Green' } else { 'Red' })
    
    if ($postgresStatus -eq "running") {
        $pgHealth = docker inspect --format='{{.State.Health.Status}}' "${ProjectName}-postgres-1" 2>$null
        Write-Host "Postgres HP:  $pgHealth" -ForegroundColor $(if ($pgHealth -eq 'healthy') { 'Green' } else { 'Yellow' })
    }
    
    Write-Host ""
    
    if ($postgresStatus -ne "running") {
        Write-Host "Postgres nao esta rodando. Inicie a stack com a opcao 3 ou 6." -ForegroundColor Yellow
    }
    elseif ($mcpStatus -ne "running") {
        Write-Host "MCP Server nao esta rodando. Verifique os logs com a opcao 8." -ForegroundColor Yellow
    }
}

function Test-Health {
    Write-Title "Health check MCP"
    Sync-Env
    $url = "http://localhost:$HostPort/healthz"
    try {
        Invoke-RestMethod -Uri $url -Method Get
    }
    catch {
        Write-Host "Falha ao chamar $url" -ForegroundColor Red
        Write-Host $_.Exception.Message
    }
}

function Test-MCP {
    Write-Title "Teste POST /mcp tools/list"
    Sync-Env
    $url = "http://localhost:$HostPort/mcp"
    $headers = @{
        "Authorization" = "Bearer $McpApiKey"
        "Accept" = "application/json, text/event-stream"
        "MCP-Protocol-Version" = "2025-06-18"
    }

    $body = @{
        jsonrpc = "2.0"
        id = 1
        method = "tools/list"
        params = @{}
    } | ConvertTo-Json -Depth 5

    try {
        Invoke-RestMethod -Uri $url -Method Post -Headers $headers -ContentType "application/json" -Body $body
    }
    catch {
        Write-Host "Falha ao chamar $url" -ForegroundColor Red
        Write-Host $_.Exception.Message
    }
}

function Test-MCPValidateSql {
    Write-Title "Teste validate_sql"
    Sync-Env
    $url = "http://localhost:$HostPort/mcp"
    $headers = @{
        "Authorization" = "Bearer $McpApiKey"
        "Accept" = "application/json, text/event-stream"
        "MCP-Protocol-Version" = "2025-06-18"
    }

    $body = @{
        jsonrpc = "2.0"
        id = 2
        method = "tools/call"
        params = @{
            name = "validate_sql"
            arguments = @{
                sql = "select 1 as ok"
                mode = "read"
            }
        }
    } | ConvertTo-Json -Depth 8

    try {
        Invoke-RestMethod -Uri $url -Method Post -Headers $headers -ContentType "application/json" -Body $body
    }
    catch {
        Write-Host "Falha ao chamar $url" -ForegroundColor Red
        Write-Host $_.Exception.Message
    }
}

function Test-MCPExecuteDdl {
    Write-Title "Teste execute_ddl (CREATE TABLE)"
    Sync-Env
    $url = "http://localhost:$HostPort/mcp"
    $headers = @{
        "Authorization" = "Bearer $McpApiKey"
        "Accept" = "application/json, text/event-stream"
        "MCP-Protocol-Version" = "2025-06-18"
    }

    $body = @{
        jsonrpc = "2.0"
        id = 3
        method = "tools/call"
        params = @{
            name = "execute_ddl"
            arguments = @{
                sql = "CREATE TABLE IF NOT EXISTS public.mcp_test (id serial PRIMARY KEY, name text, created_at timestamp DEFAULT now())"
            }
        }
    } | ConvertTo-Json -Depth 8

    try {
        $result = Invoke-RestMethod -Uri $url -Method Post -Headers $headers -ContentType "application/json" -Body $body
        Write-Host "Resultado:" -ForegroundColor Green
        $result | ConvertTo-Json -Depth 5
    }
    catch {
        Write-Host "Falha ao chamar $url" -ForegroundColor Red
        Write-Host $_.Exception.Message
    }
}

function Write-StackInfo {
    Sync-Env
    Write-Host ""
    Write-Host "Stack iniciada." -ForegroundColor Green
    Write-Host "Postgres:     postgres://localhost:5432/${POSTGRES_DB:-mcp}" -ForegroundColor Green
    Write-Host "MCP Endpoint: http://localhost:$HostPort/mcp" -ForegroundColor Green
    Write-Host "Health:       http://localhost:$HostPort/healthz" -ForegroundColor Green
    Write-Host "API Key:      $McpApiKey" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Use este token como Authorization: Bearer <token>" -ForegroundColor Yellow
}

function Show-Menu {
    Sync-Env
    Clear-Host
    Write-FelsenBanner
    Write-Host ""
    Write-Host "MCP Postgres - Docker Dev" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "Projeto:       $ProjectRoot"
    Write-Host "Compose:       $ComposeFile"
    Write-Host "Dockerfile:    $Dockerfile"
    Write-Host "Env:           $EnvFile"
    Write-Host "Imagem:        $ImageName`:$ImageTag"
    Write-Host "Porta MCP:     $HostPort"
    Write-Host "Porta PG:      5432"
    Write-Host "API Key:       $(if ($McpApiKey) { 'configurado' } else { 'sera gerada' })"
    Write-Host ""
    Write-Host "1. Buildar imagem Docker"
    Write-Host "2. Buildar stack"
    Write-Host "3. Subir stack"
    Write-Host "4. Parar stack"
    Write-Host "5. Reiniciar stack"
    Write-Host "6. Recriar stack"
    Write-Host "7. Ver status"
    Write-Host "8. Ver logs"
    Write-Host "9. Health check MCP"
    Write-Host "10. Verificar saude da stack"
    Write-Host "11. Testar /mcp tools/list"
    Write-Host "12. Testar validate_sql"
    Write-Host "13. Testar execute_ddl"
    Write-Host "14. Ver config compose"
    Write-Host "15. Liberar portas no Firewall"
    Write-Host "16. Remover stack e volumes"
    Write-Host "17. Salvar .env"
    Write-Host "0. Sair"
    Write-Host ""
}

do {
    Show-Menu
    $choice = Read-Host "Escolha uma opcao"

    try {
        switch ($choice) {
            "1" { Invoke-ImageBuild }
            "2" { Invoke-StackBuild }
            "3" { Start-Stack }
            "4" { Stop-Stack }
            "5" { Restart-Stack }
            "6" { Recreate-Stack }
            "7" { Show-Status }
            "8" { Show-Logs }
            "9" { Test-Health }
            "10" { Test-StackHealth }
            "11" { Test-MCP }
            "12" { Test-MCPValidateSql }
            "13" { Test-MCPExecuteDdl }
            "14" { Show-ComposeConfig }
            "15" { Enable-WindowsFirewallPorts }
            "16" { Remove-StackVolumes }
            "17" { Save-EnvFile }
            "0" { break }
            default { Write-Host "Opcao invalida." -ForegroundColor Yellow }
        }
    }
    catch {
        Write-Host ""
        Write-Host "Erro: $($_.Exception.Message)" -ForegroundColor Red
    }

    if ($choice -ne "0" -and $choice -ne "7") {
        Write-Host ""
        Read-Host "Pressione Enter para continuar"
    }
} while ($choice -ne "0")
