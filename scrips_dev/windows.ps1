param(
    [string]$ImageName = "mcp-postgres",
    [int]$HostPort = 8080,
    [string]$BearerToken = "",
    [string]$AllowedOrigins = "",
    [string]$DatabaseUrl = "",
    [string]$WriterBearerToken = "",
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
$TokenDir = Join-Path $env:LOCALAPPDATA "FelsenTechnologies\mcp-postgres"
$TokenFile = Join-Path $TokenDir "bearer-token.txt"
$GeneratedBearerToken = $false
$LastComposeSucceeded = $false

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
        Write-Host "Docker nao esta disponivel. Abra o Docker Desktop e tente novamente." -ForegroundColor Red
        return $false
    }
}

function Test-ComposeFile {
    if (-not (Test-Path -LiteralPath $script:ComposeFile)) {
        Write-Host "Arquivo docker compose nao encontrado:" -ForegroundColor Red
        Write-Host $script:ComposeFile -ForegroundColor Yellow
        return $false
    }
    return $true
}

function New-BearerToken {
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
    }
    finally {
        $rng.Dispose()
    }
    return [Convert]::ToBase64String($bytes).TrimEnd("=").Replace("+", "-").Replace("/", "_")
}

function Save-BearerToken {
    if ([string]::IsNullOrWhiteSpace($script:BearerToken)) { return }
    if (-not (Test-Path -LiteralPath $script:TokenDir)) {
        New-Item -ItemType Directory -Path $script:TokenDir -Force | Out-Null
    }
    Set-Content -LiteralPath $script:TokenFile -Value $script:BearerToken -NoNewline
}

function Initialize-BearerToken {
    if ([string]::IsNullOrWhiteSpace($script:BearerToken)) {
        if (Test-Path -LiteralPath $script:TokenFile) {
            $savedToken = (Get-Content -LiteralPath $script:TokenFile -Raw).Trim()
            if (-not [string]::IsNullOrWhiteSpace($savedToken)) {
                $script:BearerToken = $savedToken
                $script:GeneratedBearerToken = $false
                return
            }
        }

        $script:BearerToken = New-BearerToken
        $script:GeneratedBearerToken = $true
        Save-BearerToken
    }
}

function Get-CurrentDatabaseUrl {
    if (-not [string]::IsNullOrWhiteSpace($script:DatabaseUrl)) {
        return $script:DatabaseUrl
    }
    if (-not [string]::IsNullOrWhiteSpace($env:CRM_DATABASE_URL)) {
        return $env:CRM_DATABASE_URL
    }
    if (-not [string]::IsNullOrWhiteSpace($env:DATABASE_URL)) {
        return $env:DATABASE_URL
    }
    return ""
}

function Read-ValueOrDefault {
    param(
        [string]$Prompt,
        [string]$CurrentValue,
        [string]$DefaultValue = ""
    )

    $suffix = ""
    if (-not [string]::IsNullOrWhiteSpace($CurrentValue)) {
        $suffix = " [$CurrentValue]"
    }
    elseif (-not [string]::IsNullOrWhiteSpace($DefaultValue)) {
        $suffix = " [$DefaultValue]"
    }

    $value = Read-Host "$Prompt$suffix"
    if (-not [string]::IsNullOrWhiteSpace($value)) {
        return $value.Trim()
    }
    if (-not [string]::IsNullOrWhiteSpace($CurrentValue)) {
        return $CurrentValue
    }
    return $DefaultValue
}

function Read-RequiredValue {
    param(
        [string]$Prompt,
        [string]$CurrentValue,
        [string]$Example = ""
    )

    do {
        if (-not [string]::IsNullOrWhiteSpace($Example)) {
            Write-Host "Exemplo: $Example" -ForegroundColor DarkGray
        }
        $value = Read-ValueOrDefault -Prompt $Prompt -CurrentValue $CurrentValue
        if (-not [string]::IsNullOrWhiteSpace($value)) {
            return $value
        }
        Write-Host "Valor obrigatorio." -ForegroundColor Yellow
    } while ($true)
}

function Prompt-StackVariables {
    Write-Title "Variaveis da stack"
    Write-Host "Pressione Enter para manter o valor atual mostrado entre colchetes." -ForegroundColor Yellow
    Write-Host ""

    $script:DatabaseUrl = Read-RequiredValue `
        -Prompt "CRM_DATABASE_URL / DATABASE_URL" `
        -CurrentValue (Get-CurrentDatabaseUrl) `
        -Example "postgres://user:password@host.docker.internal:5432/crm?sslmode=disable"

    do {
        $portValue = Read-ValueOrDefault -Prompt "Porta HTTP local" -CurrentValue "$script:HostPort" -DefaultValue "8080"
        $parsedPort = 0
        if ([int]::TryParse($portValue, [ref]$parsedPort) -and $parsedPort -gt 0 -and $parsedPort -le 65535) {
            $script:HostPort = $parsedPort
            break
        }
        Write-Host "Informe uma porta valida entre 1 e 65535." -ForegroundColor Yellow
    } while ($true)

    Initialize-BearerToken
    $previousBearerToken = $script:BearerToken
    $script:BearerToken = Read-ValueOrDefault -Prompt "MCP_API_KEY reader" -CurrentValue $script:BearerToken
    if ($script:BearerToken -ne $previousBearerToken) {
        $script:GeneratedBearerToken = $false
        Save-BearerToken
    }
    $script:WriterBearerToken = Read-ValueOrDefault -Prompt "MCP_WRITER_API_KEY writer" -CurrentValue $script:WriterBearerToken -DefaultValue $script:BearerToken
    $script:AllowedOrigins = Read-ValueOrDefault -Prompt "MCP_ALLOWED_ORIGINS opcional" -CurrentValue $script:AllowedOrigins
    $script:ConfigPath = Read-ValueOrDefault -Prompt "POSTGRES_MCP_CONFIG" -CurrentValue $script:ConfigPath -DefaultValue "/app/Docker/config.docker.yaml"

    if ($script:ConfigPath -eq "configs/example.yaml") {
        Write-Host ""
        Write-Host "Aviso: configs/example.yaml escuta em 127.0.0.1 e nao e recomendado para Docker/Portainer." -ForegroundColor Yellow
        $script:ConfigPath = Read-ValueOrDefault -Prompt "Use o config Docker" -CurrentValue "/app/Docker/config.docker.yaml"
    }
}

function Write-BearerTokenInfo {
    if ([string]::IsNullOrWhiteSpace($script:BearerToken)) { return }

    Write-Host ""
    if ($script:GeneratedBearerToken) {
        Write-Host "MCP_API_KEY gerado automaticamente:" -ForegroundColor Yellow
    }
    else {
        Write-Host "MCP_API_KEY configurado:" -ForegroundColor Yellow
    }
    Write-Host $script:BearerToken -ForegroundColor Green
    Write-Host ""
    Write-Host "Use este valor na OpenAI como authorization/bearer token." -ForegroundColor Yellow
}

function Invoke-Compose {
    param([string[]]$ArgsList)
    $script:LastComposeSucceeded = $false
    if (-not (Test-Docker)) { return }
    if (-not (Test-ComposeFile)) { return }
    Push-Location $ProjectRoot
    try {
        $resolvedDatabaseUrl = Get-DatabaseUrl
        $env:HTTP_PORT = "$HostPort"
        $env:MCP_PORT = "$HostPort"
        $env:POSTGRES_MCP_PORT = "$HostPort"
        $env:IMAGE_NAME = $ImageName
        $env:HTTP_BEARER_TOKEN = $BearerToken
        $env:MCP_BEARER_TOKEN = $BearerToken
        $env:MCP_API_KEY = $BearerToken
        if ([string]::IsNullOrWhiteSpace($WriterBearerToken)) {
            $env:MCP_WRITER_API_KEY = $BearerToken
        }
        else {
            $env:MCP_WRITER_API_KEY = $WriterBearerToken
        }
        $env:POSTGRES_MCP_API_KEY = $BearerToken
        $env:MCP_ALLOWED_ORIGINS = $AllowedOrigins
        $env:POSTGRES_MCP_CONFIG = $ConfigPath
        $env:DATABASE_URL = $resolvedDatabaseUrl
        $env:CRM_DATABASE_URL = $resolvedDatabaseUrl
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

function Get-DatabaseUrl {
    if (-not [string]::IsNullOrWhiteSpace($script:DatabaseUrl)) {
        return $script:DatabaseUrl
    }
    if (-not [string]::IsNullOrWhiteSpace($env:CRM_DATABASE_URL)) {
        return $env:CRM_DATABASE_URL
    }
    if (-not [string]::IsNullOrWhiteSpace($env:DATABASE_URL)) {
        return $env:DATABASE_URL
    }
    throw "Informe -DatabaseUrl ou configure CRM_DATABASE_URL/DATABASE_URL com a DSN do Postgres existente."
}

function Invoke-ImageBuild {
    Write-Title "Build da imagem Docker"
    if (-not (Test-Docker)) { return }
    if (-not (Test-Path -LiteralPath $script:Dockerfile)) {
        throw "Dockerfile nao encontrado: $script:Dockerfile"
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
    Prompt-StackVariables
    Invoke-Compose @("build")
}

function Start-Stack {
    param([switch]$SkipPrompt)

    Write-Title "Subir stack"
    if (-not $SkipPrompt) {
        Prompt-StackVariables
    }
    Invoke-Compose @("up", "-d", "--build")
    if ($script:LastComposeSucceeded) {
        Write-BearerTokenInfo
    }
}

function Stop-Stack {
    Write-Title "Parar stack"
    Invoke-Compose @("down")
}

function Stop-StackWithVolumes {
    Write-Title "Parar stack e remover volumes"
    Invoke-Compose @("down", "--volumes")
}

function Restart-Stack {
    Prompt-StackVariables
    Stop-Stack
    Start-Stack -SkipPrompt
}

function RecreateStack {
    Write-Title "Recriar stack"
    Prompt-StackVariables
    Invoke-Compose @("up", "-d", "--build", "--force-recreate")
    if ($script:LastComposeSucceeded) {
        Write-BearerTokenInfo
    }
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

function Test-Health {
    Write-Title "Health check"
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
    Initialize-BearerToken
    $url = "http://localhost:$HostPort/mcp"
    $headers = Get-AuthHeaders
    $headers["MCP-Protocol-Version"] = "2025-06-18"

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
    Initialize-BearerToken
    $url = "http://localhost:$HostPort/mcp"
    $headers = Get-AuthHeaders
    $headers["MCP-Protocol-Version"] = "2025-06-18"

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

function Get-AuthHeaders {
    Initialize-BearerToken
    return @{
        "Authorization" = "Bearer $script:BearerToken"
        "Accept" = "application/json, text/event-stream"
    }
}

function Show-Menu {
    Clear-Host
    Write-FelsenBanner
    Write-Host ""
    Write-Host "MCP Postgres - Docker Dev" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "Projeto:       $ProjectRoot"
    Write-Host "Compose:       $ComposeFile"
    Write-Host "Config:        $ConfigPath"
    Write-Host "Dockerfile:    $Dockerfile"
    Write-Host "Imagem:        $ImageName`:$ImageTag"
    Write-Host "Porta local:   $HostPort"
    Write-Host "Database URL:  $(if ($DatabaseUrl -or $env:CRM_DATABASE_URL -or $env:DATABASE_URL) { 'configurado' } else { 'obrigatorio para subir a stack' })"
    Write-Host "Bearer token:  $(if ($BearerToken) { 'configurado' } else { 'sera gerado ao subir a stack' })"
    Write-Host "Token salvo:   $TokenFile"
    Write-Host ""
    Write-Host "1. Buildar imagem Docker"
    Write-Host "2. Buildar stack"
    Write-Host "3. Subir stack"
    Write-Host "4. Parar stack"
    Write-Host "5. Reiniciar stack"
    Write-Host "6. Recriar stack"
    Write-Host "7. Ver status"
    Write-Host "8. Ver logs"
    Write-Host "9. Health check"
    Write-Host "10. Testar /mcp tools/list"
    Write-Host "11. Testar validate_sql"
    Write-Host "12. Ver config compose"
    Write-Host "13. Parar stack e remover volumes"
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
            "6" { RecreateStack }
            "7" { Show-Status }
            "8" { Show-Logs }
            "9" { Test-Health }
            "10" { Test-MCP }
            "11" { Test-MCPValidateSql }
            "12" { Show-ComposeConfig }
            "13" { Stop-StackWithVolumes }
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
