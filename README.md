# Felsen MCP Server Postgres

Servidor MCP em Go para PostgreSQL, com Streamable HTTP, OAuth 2.1, API keys, descoberta de schema, múltiplas conexões, consultas somente leitura e operações DML/DDL explicitamente allowlisted.

Versão atual: `0.5.0`.

## Quick start

Use a real random token even for local development:

```powershell
$env:POSTGRES_MCP_CONFIG = "configs/example.yaml"
$env:DATABASE_URL = "postgres://user:password@localhost:5432/postgres?sslmode=disable"
$bytes = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
$env:MCP_API_KEY = [BitConverter]::ToString($bytes).Replace("-", "").ToLowerInvariant()
$citationBytes = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($citationBytes) } finally { $rng.Dispose() }
$env:MCP_CITATION_SIGNING_KEY = [Convert]::ToBase64String($citationBytes)
go run ./cmd/postgres-mcp
```

The default local endpoint is:

```text
http://127.0.0.1:8080/mcp
```

Every MCP request must send Authorization: Bearer <MCP_API_KEY>. The reader token remains read-only; the example configuration enables DDL and wildcard DML for the configured schemas so a write-scoped principal can manage tables, indexes, and rows. Set `MCP_DDL_ENABLED=false` or `MCP_DML_ENABLED=false` to disable either capability. Do not put tokens directly in YAML or commit real credentials.

`server.public_base_url` is required because search and fetch return absolute, user-openable citation URLs. The local Docker stack defaults it to `http://localhost:8080`; for a published deployment, set `MCP_PUBLIC_BASE_URL` to the public HTTPS origin. The example signs source URLs with `MCP_CITATION_SIGNING_KEY` for 15 minutes; keep that key secret.

## MCP/OpenAI integration

Expose a stable HTTPS MCP endpoint, preserve bearer authentication, and use the official OpenAI MCP integration fields server_url, allowed_tools, and require_approval. A safe read-only configuration starts with:

```json
{
  "type": "mcp",
  "server_label": "postgres",
  "server_url": "https://mcp.example.com/mcp",
  "allowed_tools": ["search", "fetch", "list_schemas", "describe_table"],
  "require_approval": "never"
}
```

Keep require_approval enabled for execute_dml, execute_ddl, and execute_script. The validate_* tools only inspect SQL and do not change the database. See the [OpenAI MCP API documentation](https://developers.openai.com/api/docs/mcp) and [MCP server guidance for plugins](https://developers.openai.com/plugins/build/mcp-server).

### ChatGPT connector OAuth

The server can expose an embedded OAuth 2.1 provider for the ChatGPT connector. Enable it only with a public HTTPS origin:

```text
MCP_OAUTH_ENABLED=true
MCP_OAUTH_ISSUER=https://mcp.example.com
MCP_OAUTH_RESOURCE=https://mcp.example.com
MCP_OAUTH_SIGNING_KEY=<long-random-secret-at-least-32-characters>
MCP_OAUTH_USERNAME=<connector-login>
MCP_OAUTH_PASSWORD=<long-random-password>
MCP_OAUTH_PRINCIPAL=admin
MCP_OAUTH_CALLBACK_URL=https://chatgpt.com/connector/oauth/<callback-id>
MCP_OAUTH_DEFAULT_SCOPES=read,write,ddl,admin
MCP_OAUTH_BASE_SCOPES=read,write,ddl,admin
```

Set `MCP_ADMIN_API_KEY` to create the full-access OAuth principal. Use
`MCP_OAUTH_PRINCIPAL=reader` with `read` scopes for a read-only connector.

The protected-resource metadata, authorization-server metadata, DCR, authorization, and token endpoints are respectively exposed at:

```text
/.well-known/oauth-protected-resource
/.well-known/oauth-authorization-server
/oauth/register
/oauth/authorize
/oauth/token
```

In ChatGPT's advanced authentication settings, use Dynamic Client Registration after the registration endpoint is discovered. If using User-Defined OAuth Client, use client ID `felsen-chatgpt`, leave the client secret empty, select token endpoint auth method `none`, and use the configured default/base scopes. Set `MCP_OAUTH_CALLBACK_URL` in `.env` to the exact callback URL shown by ChatGPT, then restart the server. This value is required when OAuth is enabled, and the static client compares `redirect_uri` exactly against it; no callback domain or path is hardcoded. Dynamic registrations retain the exact callback URL submitted by ChatGPT. OIDC remains intentionally disabled because this bootstrap provider does not claim an email identity.

`MCP_OAUTH_PRINCIPAL` is the name of an entry under `auth.api_keys`, not a scope. The shipped Docker configuration has a read-only `reader` principal and adds `writer`, `ddl`, or `admin` when the corresponding environment token is set. For full OAuth scope access, set `MCP_ADMIN_API_KEY`, `MCP_OAUTH_PRINCIPAL=admin`, and request `read,write,ddl,admin`. The `admin` scope does not bypass the SQL safety guard, DML policies, row limits, or schema restrictions.

Mutation defaults are controlled globally by `MCP_DDL_ENABLED` and
`MCP_DML_ENABLED`, both defaulting to `true` in the shipped Docker and local
configurations. DML uses the default policy `{schema: "*", table: "*", operations:
[insert, update, delete]}` within the configured schemas. Each connection still
enforces `max_affected_rows` (100 by default). Add narrower `dml_policies` under
`connections.<name>` in the selected YAML file, or set the flags to `false` for
a restricted deployment. `execute_script` has separate per-connection limits so
an approved import can contain up to 10,000 statements, affect up to 10,000 rows,
and contain up to 8 MiB by default. The script transaction is rolled back when
any validation, database execution, or limit check fails.

The MCP endpoint still returns a standards-compliant `401` with `resource_metadata` when no bearer is present. ChatGPT uses that challenge to discover OAuth and then calls `initialize`/`tools/list` with the access token; `tools/list` is not made public because exposing discovery must not bypass database authorization. Existing API-key bearer clients continue to work.

The embedded login is a bootstrap identity provider, not a replacement for an enterprise IdP. For production deployments with multiple users, prefer placing an external OAuth/OIDC provider in front of the MCP server and map its claims to scoped principals.

`search` follows the standard single-argument contract `{query}` and searches every connection authorized for the bearer token. It returns `{results:[{id,title,url}]}`; `fetch` accepts only `{id}` and returns `{id,title,text,url,metadata}`. IDs are opaque `pg:v1:` connection-scoped identifiers generated by the server—pass them back unchanged instead of constructing `schema.table` IDs. This keeps identical objects in different connections unambiguous. URLs point to a short-lived signed `/sources` endpoint. Bearer authentication remains accepted for direct source access.

### Atomic SQL imports

`execute_script` receives the complete SQL file content in its `script` field;
it does not read a path from the MCP client's filesystem. In particular, pass
the content of an attached `/mnt/data/*.sql` file, not the literal `/mnt/data`
path. Each statement must be an allowlisted `INSERT`, `UPDATE`, `DELETE`, or
supported DDL statement. Do not include `BEGIN`, `COMMIT`, `ROLLBACK`, `COPY`,
`DO`, `SELECT`, or other transaction/control statements because the server owns
the transaction. The response includes per-statement and aggregate affected-row
counts and records the optional `source_name` in the audit event. Use follow-up read-only `COUNT(*)` queries to certify final table counts;
the server does not hardcode or infer the expected CRM counts.

Validações de mutação usam actions separadas para manter o princípio de menor
privilégio: `validate_sql` aceita somente leitura e exige `read`, `validate_dml`
exige `write`, e `validate_ddl` exige `ddl`. O campo `mode` não faz mais parte
de `validate_sql`.

## Versioning and releases

The repository root `VERSION` file is the SemVer source of truth. It is advertised through the MCP implementation metadata, the `version`/`--version` CLI commands, and Docker build metadata. On `main`, a push that changes `VERSION` runs the release workflow, validates `X.Y.Z`, and creates the matching annotated tag `vX.Y.Z` without overwriting an existing tag. The Windows development script also exposes local tag creation as menu option 19.

## Tools

- list_connections
- list_schemas
- list_tables
- describe_table
- sample_rows
- validate_sql
- validate_dml
- validate_ddl
- execute_sql
- execute_dml
- execute_ddl
- execute_script
- explain_sql
- refresh_schema_cache
- search
- fetch

## Resources

- postgres://connections
- postgres://{connection}/schema
- postgres://{connection}/schemas
- postgres://{connection}/tables
- postgres://{connection}/table/{schema}/{table}

## Safety and operational defaults

- Bearer authentication is required for MCP and SSE; source URLs use short-lived signatures or Bearer authentication, and bare tokens are rejected. When OAuth is enabled, the 401 challenge advertises the protected-resource metadata URL.
- API keys must declare read, write, ddl, or admin scopes and allowed connections.
- YAML/JSON unknown fields fail configuration loading.
- The SQL guard is conservative and denies comments, dollar-quoted strings, multiple statements/relation lists in single-statement tools, row-locking clauses, dangerous functions, CTEs, and non-allowlisted DDL. `execute_script` is the controlled exception for multi-statement imports: it strips only envelope comments, validates each statement independently, and executes the complete allowlisted script atomically.
- Read SQL runs in a PostgreSQL READ ONLY transaction.
- SQL result rows are capped server-side, including queries that already contain a larger LIMIT.
- DML is enabled by the shipped wildcard policy for configured schemas and rolls back when `max_affected_rows` (100 by default) is exceeded; DDL is enabled by default in the shipped configurations, supports common table/index/constraint operations, rejects destructive CASCADE and multi-object operations, and permits CASCADE only as a foreign-key referential action.
- Results are masked using sensitive-column patterns unless explicitly allowed by connection configuration.
- HTTP body size (8 MiB by default for attached scripts), per-script statement/row/byte limits, request concurrency, read/write/idle timeouts, readiness checks, and audit write errors are enforced.

## Docker

Copy .env.example to .env, replace every replace-with-* value, and set MCP_PUBLIC_BASE_URL. Compose fails closed when database credentials, the reader token, or the citation signing key are missing. The Docker configuration enables mutation tools for write-scoped principals, remains restricted to the public schema by default, and keeps masking enabled.

```powershell
Copy-Item .env.example .env
docker compose --env-file .env --file Docker/docker-compose.yaml config
docker compose --env-file .env --file Docker/docker-compose.yaml up --build -d
```

The container image is distroless and includes a native healthcheck command. The Swarm stack does not publish the Postgres port externally.

## Verification

```powershell
$env:CGO_ENABLED = "0"
go test ./...
go vet ./...
go mod verify
go build -trimpath ./cmd/postgres-mcp
git diff --check
```

CI also runs the race detector, formatting checks, and a Docker build. When using CGO_ENABLED=0, the project uses the conservative non-CGO statement lexer; CI validates the CGO parser-backed build as well. No Windows Docker test is possible when Docker Desktop/CLI is unavailable.


## Guia completo de setup e operação

Esta seção reúne o fluxo recomendado para desenvolvimento local, Docker Desktop,
integração com ChatGPT/OpenAI, importações SQL, operação com múltiplas conexões,
produção e diagnóstico. Os arquivos de configuração de referência são
`.env.example`, `configs/example.yaml` e `Docker/config.docker.yaml`.

### O que o projeto faz

O serviço é um gateway MCP stateless para PostgreSQL:

1. O cliente MCP autentica com Bearer API key ou OAuth 2.1.
2. O servidor identifica o principal, os escopos e as conexões permitidas.
3. A camada MCP expõe tools e resources com schemas JSON.
4. A camada SQL valida o comando antes de chegar ao PostgreSQL.
5. A store executa leitura, DML ou DDL respeitando limites, políticas e masking.
6. A auditoria registra a operação sem registrar SQL ou tokens em claro.
7. `search` e `fetch` produzem IDs e citações que preservam a conexão de origem.

Fluxo resumido:

```text
cliente MCP/ChatGPT
        │ HTTPS + Bearer/OAuth
        ▼
/mcp ── autenticação ── autorização por escopo/conexão
        │
        ├── tools/resources MCP
        ├── SQL guard + DML/DDL policies
        ├── PostgreSQL store + schema cache
        └── auditoria + citações assinadas
```

O servidor não lê arquivos no computador do cliente. Para importações, o cliente
precisa enviar o conteúdo SQL na propriedade `script`.

### Estrutura do repositório

| Caminho | Responsabilidade |
| --- | --- |
| `cmd/postgres-mcp` | entrada do processo HTTP, middleware, OAuth, healthchecks e shutdown |
| `internal/config` | carregamento YAML/JSON, variáveis de ambiente e validação fail-closed |
| `internal/authn` | API keys, principals, escopos e conexões permitidas |
| `internal/oauth` | metadados OAuth, DCR, autorização, token, refresh e PKCE |
| `internal/mcpserver` | registro das tools/resources e contrato MCP |
| `internal/sqlguard` | parser/validador de SELECT, DML, DDL e scripts |
| `internal/postgres` | pool, introspecção, execução, limites e transações |
| `internal/audit` | eventos estruturados em stdout ou arquivo |
| `internal/version` | versão, commit e data de build |
| `configs` | configuração local de referência |
| `Docker` | Dockerfile, Compose, Swarm e configuração de container |
| `scrips_dev/windows.ps1` | menu de operação e testes no Windows |
| `.github/workflows` | CI, build de imagem e criação automática de tags |

### Pré-requisitos

Para executar localmente:

- Go na versão declarada em `go.mod`;
- PostgreSQL acessível pela máquina;
- PowerShell 5.1 ou PowerShell 7 no Windows;
- Git.

Para Docker Desktop:

- Docker Desktop iniciado;
- Docker Compose v2 disponível como `docker compose`;
- pelo menos uma conexão PostgreSQL, local ou externa.

Para produção:

- endpoint HTTPS estável atrás de proxy reverso ou load balancer;
- segredo separado para cada API key e para assinatura;
- imagem publicada com tag imutável;
- PostgreSQL sem porta pública desnecessária.

### Setup local sem Docker

1. Copie ou adapte a configuração local:

```powershell
$env:POSTGRES_MCP_CONFIG = "configs/example.yaml"
$env:DATABASE_URL = "postgres://usuario:senha@localhost:5432/postgres?sslmode=disable"
```

2. Gere segredos temporários para desenvolvimento. Não reutilize estes valores
em produção:

```powershell
$bytes = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
$env:MCP_API_KEY = [BitConverter]::ToString($bytes).Replace("-", "").ToLowerInvariant()

$citationBytes = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($citationBytes) } finally { $rng.Dispose() }
$env:MCP_CITATION_SIGNING_KEY = [Convert]::ToBase64String($citationBytes)
```

3. Execute com o lexer conservador sem CGO, que é o mesmo modo usado pela
imagem Docker:

```powershell
$env:CGO_ENABLED = "0"
go run ./cmd/postgres-mcp
```

4. Valide a disponibilidade:

```powershell
Invoke-WebRequest http://127.0.0.1:8080/healthz
Invoke-WebRequest http://127.0.0.1:8080/readyz
```

`/healthz` confirma que o processo está vivo. `/readyz` também verifica a
conectividade com as conexões configuradas. O endpoint MCP local padrão é
`http://127.0.0.1:8080/mcp`. Use `Authorization: Bearer <token>` em todas as
chamadas MCP e não coloque o token em arquivos versionados.

O valor de `server.public_base_url` é a origem usada para citações. Em local,
`http://127.0.0.1:8080` é suficiente. Em produção, use a origem HTTPS pública,
por exemplo `https://mcp.example.com`, sem acrescentar `/mcp`.

### Setup com Docker Desktop no Windows

1. Crie o arquivo de ambiente:

```powershell
Copy-Item .env.example .env
```

2. Edite `.env` e substitua todos os valores `replace-with-*`. Para o Compose
local, `DATABASE_URL` deve usar o nome do serviço `postgres`:

```text
DATABASE_URL=postgres://postgres:SENHA@postgres:5432/mcp?sslmode=disable
MCP_PUBLIC_BASE_URL=http://localhost:8080
```

Se o banco estiver no Windows host, use `host.docker.internal` no DSN. Se o
banco estiver fora do Docker, use o hostname ou IP privado correto. Não use
`localhost` no DSN do container para alcançar o host.

3. Renderize a configuração e confirme que os placeholders foram removidos:

```powershell
docker compose --env-file .env --file Docker/docker-compose.yaml config
```

Esse comando pode exibir valores de ambiente no terminal; não copie a saída
para tickets ou logs públicos.

4. Construa e inicie:

```powershell
docker compose --env-file .env --file Docker/docker-compose.yaml up --build -d
docker compose --env-file .env --file Docker/docker-compose.yaml ps
```

5. Verifique logs e endpoints:

```powershell
docker compose --env-file .env --file Docker/docker-compose.yaml logs -f postgres-mcp
Invoke-WebRequest http://localhost:8080/healthz
Invoke-WebRequest http://localhost:8080/readyz
```

Comandos úteis para reiniciar ou parar:

```powershell
docker compose --env-file .env --file Docker/docker-compose.yaml restart postgres-mcp
docker compose --env-file .env --file Docker/docker-compose.yaml down
```

`docker compose down` preserva os volumes nomeados. Não use `down -v` em um
ambiente com dados que precisam ser preservados: essa opção remove o volume do
PostgreSQL e o armazenamento de clientes OAuth.

A imagem usa build multi-stage e executa como usuário sem privilégios. O
Compose local expõe PostgreSQL apenas para desenvolvimento; remova o
mapeamento de porta antes de publicar esse arquivo em produção.

### Script de desenvolvimento para Windows

O caminho do script é intencionalmente `scrips_dev`. Execute:

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
./scrips_dev/windows.ps1
```

Também é possível fornecer os valores principais na chamada:

```powershell
./scrips_dev/windows.ps1 -DatabaseUrl "postgres://postgres:SENHA@host.docker.internal:5432/postgres?sslmode=disable" -McpApiKey "TOKEN_DE_LEITURA" -CitationSigningKey "CHAVE_DE_ASSINATURA_COM_32_OU_MAIS_CARACTERES" -PublicBaseUrl "http://localhost:8080"
```

O script sincroniza o `.env`, passa versão/commit/data de build ao Docker e
oferece o menu para build, subir, reiniciar, testar health, testar
`tools/list`, testar `validate_sql`, testar DDL, visualizar a configuração,
ver OAuth e criar tag. A opção 16 remove stack e volumes: use-a somente quando
a perda dos volumes for intencional. A opção 19 cria a tag SemVer e pergunta
se deve enviá-la ao remoto.

### Configuração por camadas

A configuração possui três camadas:

- `server`: endereço, endpoint, timeouts, limites HTTP, citações e concorrência;
- `auth`/`oauth`: identidade, escopos, conexão permitida e fluxo OAuth;
- `connections`: DSN, schemas, limites SQL, policies, masking e pool.

Campos importantes:

| Campo | Uso |
| --- | --- |
| `server.endpoint` | normalmente `/mcp`; não use um caminho reservado |
| `server.public_base_url` | origem absoluta para citações; sem `/mcp` |
| `server.max_body_bytes` | limite HTTP, 8 MiB por padrão |
| `connections.<nome>.schemas` | schemas que o principal pode consultar ou alterar |
| `connections.<nome>.max_rows` | limite de linhas retornadas |
| `connections.<nome>.max_affected_rows` | limite por comando DML |
| `connections.<nome>.max_script_statements` | limite de statements no script |
| `connections.<nome>.max_script_affected_rows` | limite agregado de linhas afetadas |
| `connections.<nome>.max_script_bytes` | limite de bytes do conteúdo SQL |
| `connections.<nome>.ddl_enabled` | habilita/desabilita DDL nessa conexão |
| `connections.<nome>.dml_policies` | tabelas e operações DML permitidas |
| `connections.<nome>.masking` | proteção de colunas sensíveis |
| `audit.destination` | `stdout` ou caminho configurado para auditoria |

Os nomes das conexões são parte da autorização. Um token pode acessar
`default`, um conjunto explícito de nomes ou `*`. O servidor não deve receber
credenciais de banco dentro dos argumentos MCP; elas ficam em variáveis
referenciadas por `dsn_env`.

Para dar acesso amplo de gestão a um principal, configure simultaneamente:

```yaml
ddl_enabled: true
dml_policies:
  - schema: "*"
    table: "*"
    operations: [insert, update, delete]
```

e conceda os escopos `read`, `write`, `ddl` e `admin` ao principal adequado.
Isso não remove o SQL guard, os limites, o masking ou as restrições de schema.
Em produção, prefira policies por tabela em vez do wildcard.

### Principals e escopos

O arquivo de exemplo cria:

| Principal | Variável | Escopos típicos | Uso |
| --- | --- | --- | --- |
| `reader` | `MCP_API_KEY` | `read` | descoberta e consultas |
| `writer` | `MCP_WRITER_API_KEY` | `read,write` | DML |
| `ddl` | `MCP_DDL_API_KEY` | `read,ddl` | DDL |
| `admin` | `MCP_ADMIN_API_KEY` | `read,write,ddl,admin` | gestão ampla |

Os três últimos são adicionados somente quando a variável correspondente está
preenchida. `MCP_OAUTH_PRINCIPAL` não é um scope e não pode ser um nome
arbitrário: ele precisa coincidir com um principal existente em `auth.api_keys`.
Por isso `MCP_OAUTH_PRINCIPAL=admin` exige `MCP_ADMIN_API_KEY` ou uma entrada
`admin` explicitamente declarada no YAML.

### Integração com OpenAI e ChatGPT

Para um conector HTTP, o valor apresentado como Server URL é o endpoint MCP
completo:

```text
https://mcp.example.com/mcp
```

`MCP_PUBLIC_BASE_URL` é diferente: ele contém somente a origem pública:

```text
MCP_PUBLIC_BASE_URL=https://mcp.example.com
```

Não acrescente `/mcp` ao `MCP_PUBLIC_BASE_URL`. O reverse proxy deve encaminhar
`/mcp` para a porta HTTP do container e preservar HTTPS, Host e os métodos
POST/GET usados pelo transporte Streamable HTTP.

A integração por API key envia:

```http
Authorization: Bearer TOKEN
```

A descoberta de tools permanece protegida. Sem credencial, o endpoint devolve
`401` e, quando OAuth está ativo, o challenge informa
`resource_metadata`. Depois de autenticar, o cliente pode executar
`initialize` e `tools/list`. Tornar `tools/list` público faria a descoberta
ignorar a autorização por principal.

#### OAuth 2.1 embutido

Ative OAuth somente quando a origem pública estiver publicada em HTTPS:

```dotenv
MCP_OAUTH_ENABLED=true
MCP_OAUTH_ISSUER=https://mcp.example.com
MCP_OAUTH_RESOURCE=https://mcp.example.com
MCP_OAUTH_SIGNING_KEY=SEGREDO_ALEATORIO_DIFERENTE_DA_API_KEY
MCP_OAUTH_USERNAME=login-do-conector
MCP_OAUTH_PASSWORD=senha-forte-do-conector
MCP_OAUTH_PRINCIPAL=admin
MCP_OAUTH_CLIENT_ID=felsen-chatgpt
MCP_OAUTH_DEFAULT_SCOPES=read,write,ddl,admin
MCP_OAUTH_BASE_SCOPES=read,write,ddl,admin
MCP_OAUTH_CALLBACK_URL=https://chatgpt.com/connector/oauth/ID_EXATO
MCP_ADMIN_API_KEY=TOKEN_ADMIN_ALEATORIO
```

O callback precisa ser exatamente o valor exibido pelo ChatGPT, incluindo
protocolo, domínio, caminho e qualquer identificador. A comparação é literal;
um callback de outro conector, uma barra adicional ou um valor antigo causa
`redirect_uri is not registered for this client`. Após alterar o .env, recrie
ou reinicie o container e refaça a descoberta do conector.

Endpoints OAuth públicos:

| Endpoint | Função |
| --- | --- |
| `/.well-known/oauth-protected-resource` | metadata do recurso protegido |
| `/.well-known/oauth-authorization-server` | metadata do authorization server |
| `/oauth/register` | Dynamic Client Registration |
| `/oauth/authorize` | autorização e consentimento |
| `/oauth/token` | troca de code/refresh por token |

No formulário avançado do ChatGPT:

- em Dynamic Client Registration, use a Registration URL descoberta no endpoint
  `/oauth/register`;
- em User-Defined OAuth Client, use o `MCP_OAUTH_CLIENT_ID` configurado e
  deixe o client secret vazio;
- use token endpoint auth method `none`;
- solicite somente scopes concedidos ao principal;
- mantenha PKCE S256 habilitado quando essa opção for exibida;
- o servidor não anuncia CIMD nem OIDC neste bootstrap provider.

`MCP_OAUTH_PRINCIPAL` deve coincidir com um principal de `auth.api_keys`. Para
`admin`, `MCP_ADMIN_API_KEY` é obrigatório. Se a intenção for somente leitura,
use `MCP_OAUTH_PRINCIPAL=reader` e solicite apenas `read`. Os scopes são
permissões; `admin` como valor do principal não cria o principal sozinho.

O armazenamento DCR fica em `/app/data/oauth-clients.json` no Docker. Preserve
o volume `oauth-client-data` durante upgrades para não perder registrations.
Rotacione as chaves e a senha; não registre tokens OAuth em logs.

### Contrato das tools

Todas as tools respeitam a autorização do bearer. O campo
`connection_name` é opcional nas tools que o exibem quando há uma única conexão
configurada; com várias conexões, informe o nome salvo quando a tool o exigir.
`search` e `fetch` seguem o contrato de argumento único esperado pelo ChatGPT.

| Tool | Escopo | Argumentos principais | Comportamento |
| --- | --- | --- | --- |
| `list_connections` | read | nenhum | lista conexões autorizadas |
| `list_schemas` | read | `connection_name` | lista schemas permitidos |
| `list_tables` | read | conexão, `schema` | lista tabelas/views |
| `describe_table` | read | conexão, schema, tabela | colunas, PK, FKs e índices |
| `sample_rows` | read | conexão, schema, tabela, limite | amostra com masking |
| `search` | read | apenas `query` | busca em todas as conexões autorizadas |
| `fetch` | read | apenas `id` | carrega o objeto retornado por search |
| `validate_sql` | read | conexão opcional, `sql` | valida somente SELECT; não possui `mode` |
| `validate_dml` | write | conexão opcional, `sql` | valida INSERT/UPDATE/DELETE sem executar |
| `validate_ddl` | ddl | conexão opcional, `sql` | valida DDL allowlisted sem executar |
| `execute_sql` | read | conexão opcional, `sql` | executa SELECT em transação READ ONLY |
| `execute_dml` | write | conexão opcional, `sql` | executa DML com limite e policy |
| `execute_ddl` | ddl | conexão opcional, `sql` | executa DDL permitido |
| `execute_script` | write + ddl | `script`, conexão e `source_name` opcionais | executa script DML/DDL atomicamente |
| `explain_sql` | read | conexão opcional, `sql` | executa EXPLAIN JSON de SELECT |
| `refresh_schema_cache` | read | `connection_name` | limpa cache da conexão |

`validate_sql` não aceita mais o parâmetro `mode`. Use a tool cujo escopo
corresponde ao tipo de comando. Isso evita que o GPT solicite write/ddl para uma
validação de leitura e permite que o descriptor de cada action anuncie o menor
escopo necessário.

#### search/fetch com múltiplas conexões

A chamada padrão é:

```json
{"query":"customer"}
```

A resposta contém IDs opacos no formato `pg:v1:...`. O payload do ID inclui a
conexão, schema, tabela e, quando aplicável, coluna. O cliente deve enviar o ID
de volta sem modificá-lo:

```json
{"id":"pg:v1:ID_RETORNADO_POR_SEARCH"}
```

Não construa IDs como `public.customers` e não envie `connection_name` para
`search` ou `fetch`. Assim, objetos homônimos em `crm` e `analytics` continuam
sem ambiguidade. O bearer ainda precisa ter acesso à conexão codificada no ID.

### Execução e importação SQL

Para uma única operação, siga o ciclo:

1. use `validate_sql`, `validate_dml` ou `validate_ddl` conforme o comando;
2. confirme o escopo e a policy;
3. peça aprovação para tools mutáveis;
4. execute com a tool correspondente;
5. valide o resultado com uma consulta read-only.

Exemplos aceitos pelo guard, conforme policy e schema:

- `INSERT ... ON CONFLICT DO NOTHING`;
- `INSERT ... ON CONFLICT DO UPDATE`;
- `CREATE INDEX`;
- `ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY ... REFERENCES`;
- ações referenciais `ON DELETE CASCADE` e `ON DELETE SET NULL`;
- subconsultas dentro de DML quando o parser e a policy permitirem.

Operações destrutivas continuam restritas: `DROP ... CASCADE`, múltiplos
objetos em um único comando, funções perigosas, comentários/dollar quoting,
CTEs e comandos de controle não são liberados pelo simples fato de o principal
ser admin.

#### execute_script

`execute_script` é a tool exposta para importações. Envie o texto integral no
campo `script`:

```json
{
  "connection_name": "default",
  "source_name": "felsen_crm_import_public_350_leads.sql",
  "script": "CREATE TABLE ...; INSERT INTO public.companies (...) VALUES (...);"
}
```

`source_name` é apenas um rótulo não sensível para auditoria, limitado a 256
caracteres e nunca interpretado como caminho. O servidor não consegue abrir
`/mnt/data`, `C:/temp` ou qualquer arquivo que exista apenas no ambiente do
cliente. Leia o arquivo no cliente e envie seu conteúdo.

O servidor:

1. limita o tamanho HTTP e o tamanho do script;
2. remove somente comentários de envelope aceitos;
3. divide o conteúdo em statements;
4. valida cada statement como DML ou DDL;
5. inicia uma transação;
6. executa tudo na ordem recebida;
7. faz rollback em qualquer falha ou limite excedido;
8. registra contagens e o resultado agregado na auditoria.

Não inclua `BEGIN`, `COMMIT`, `ROLLBACK`, `SELECT`, `COPY`, `DO`, `SET` ou
outros comandos de controle no script; a transação pertence ao servidor. Por
padrão, cada conexão permite 10.000 statements, 10.000 linhas afetadas e 8 MiB
de conteúdo. O limite HTTP padrão também é 8 MiB. Aumente
`max_script_statements`, `max_script_affected_rows`, `max_script_bytes` e
`server.max_body_bytes` em conjunto somente após revisar o risco operacional.

Depois da importação, certifique as contagens com consultas separadas usando
`execute_sql`. A tool não presume contagens de um CRM nem declara carga
concluída sem uma resposta observável do banco.

### Segurança e operação

A segurança é feita em camadas:

- publique somente o MCP atrás de TLS; não exponha PostgreSQL na internet;
- use API keys longas, aleatórias e distintas para reader, writer, DDL e admin;
- mantenha DSN, OAuth password, signing keys e tokens fora do YAML e do Git;
- dê ao usuário PostgreSQL somente os privilégios necessários para as schemas
  selecionadas; não use superuser como padrão;
- mantenha masking habilitado e revise padrões de colunas sensíveis;
- mantenha aprovação para `execute_dml`, `execute_ddl` e `execute_script`;
- trate DDL e DML como mudanças de produção mesmo quando o bearer for admin;
- proteja stdout e arquivos de auditoria, pois eles contêm nomes de operações,
  conexões e objetos;
- não coloque credenciais em screenshots, issues, prompts ou mensagens de
  commit;
- limite a origem CORS a hosts conhecidos quando o proxy puder fornecer essa
  política; `*` só é apropriado em um ambiente controlado;
- use healthcheck/readiness e timeouts para impedir conexões presas.

A autenticação não substitui a autorização do banco. O principal precisa ter o
scope e a conexão, a policy precisa permitir o objeto e o banco precisa
autorizar a operação.

### Deploy com Docker Swarm ou Portainer

O arquivo de produção é `Docker/docker-compose.swarm.yaml`. A imagem deve ser
construída e publicada antes do deploy:

```powershell
$version = (Get-Content VERSION -Raw).Trim()
docker build --file Docker/Dockerfile --build-arg VERSION_OVERRIDE=$version --tag mcp-postgres:v$version .
docker tag mcp-postgres:v$version registry.example.com/felsen/mcp-postgres:v$version
docker push registry.example.com/felsen/mcp-postgres:v$version
```

No Portainer, informe as variáveis obrigatórias, use
`POSTGRES_MCP_CONFIG=/app/Docker/config.docker.yaml` e escolha a mesma imagem/tag
publicada:

```text
IMAGE_NAME=registry.example.com/felsen/mcp-postgres
IMAGE_TAG=v0.5.0
MCP_PUBLIC_BASE_URL=https://mcp.example.com
DATABASE_URL=postgres://postgres:SENHA@postgres:5432/mcp?sslmode=disable
```

Com Docker Swarm:

```powershell
docker stack deploy --compose-file Docker/docker-compose.swarm.yaml mcp-stack
docker stack services mcp-stack
docker service logs --follow mcp-stack_postgres-mcp
```

O Swarm usa a rede overlay e mantém o volume de clientes OAuth. Não remova o
volume durante uma atualização. Publique apenas a porta do MCP no proxy reverso,
termine TLS no proxy e encaminhe para a porta 8080 do serviço. O Postgres deve
ficar somente na rede interna.

A release é dirigida pelo arquivo `VERSION` em SemVer estável `X.Y.Z`. O CI
valida testes, vet, build e imagem. Um push para `main` que altere `VERSION`
aciona o workflow que cria a tag anotada `vX.Y.Z`; a tag existente nunca é
sobrescrita. Use tags imutáveis no Swarm e não dependa de `latest` em produção.

### Diagnóstico rápido

| Sintoma | Verificação e correção |
| --- | --- |
| `server.public_base_url is required` | defina `MCP_PUBLIC_BASE_URL` como uma origem absoluta; em produção use HTTPS e não inclua `/mcp` |
| `oauth.principal "admin" does not match an auth api key` | defina `MCP_ADMIN_API_KEY` ou declare um principal `admin` no YAML |
| `redirect_uri is not registered for this client` | copie o callback exato do ChatGPT para `MCP_OAUTH_CALLBACK_URL` e reinicie/recrie o container |
| ChatGPT não cria o connector | confirme HTTPS, `/mcp` no Server URL, metadata OAuth e o checkbox de risco; depois faça nova descoberta |
| OAuth pede escopos indisponíveis | confirme os scopes do principal e se `validate_dml`/`validate_ddl` estão sendo anunciados para ele |
| `execute_script` não aparece | use imagem nova, confirme `tools/list` autenticado, escopos write+ddl, aprovação e reconecte o connector |
| `FORBIDDEN: developer MCPs` | esse bloqueio pertence ao ambiente do cliente/conversa; valide o endpoint e use uma conversa que aceite MCPs de desenvolvimento |
| `ALTER TABLE set is not allowed` com `ON DELETE SET NULL` | use a imagem que contém o parser corrigido; DDL deve ser validado por `validate_ddl` antes da execução |
| script diz que não encontra `/mnt/data` | envie o conteúdo SQL no campo `script`; nunca um caminho local do cliente |
| resultados de `search` e `fetch` são ambíguos | use IDs `pg:v1:...` retornados pelo servidor sem editá-los; não construa IDs manualmente |
| erro de conexão com Docker PostgreSQL | dentro do Compose use hostname `postgres`; para banco no host Windows use `host.docker.internal` |
| `/readyz` falha | verifique DSN, healthcheck, credenciais e se o usuário tem acesso ao database/schema |
| mudanças de .env não surtiram efeito | recrie o container com `up --build -d` ou faça `docker compose recreate postgres-mcp` |
| Docker não disponível no Windows | abra Docker Desktop, aguarde `docker version` responder e execute novamente o menu/script |

Quando um connector mantém tools antigas, remova e recrie a conexão ou force uma
nova listagem de tools. O descriptor é cacheado pelo cliente e pode não refletir
uma imagem recém-publicada imediatamente.

### Testes e validação antes de liberar

Execute no PowerShell a partir da raiz:

```powershell
$env:CGO_ENABLED = "0"
go test ./...
go vet ./...
go mod verify
go build -trimpath ./cmd/postgres-mcp
if ((gofmt -l .).Count -ne 0) { throw "Existem arquivos Go sem gofmt" }
git diff --check
```

Para exercitar o parser CGO-backed, use uma máquina Linux/CI com compilador C e
execute também `CGO_ENABLED=1 go test ./...`. A imagem Docker é construída com
`CGO_ENABLED=0` e usa o lexer conservador. A suíte de integração PostgreSQL é
opcional e roda quando `POSTGRES_MCP_TEST_DSN` estiver definido:

```powershell
$env:POSTGRES_MCP_TEST_DSN = "postgres://usuario:senha@localhost:5432/postgres?sslmode=disable"
go test ./internal/postgres -run Integration -count=1
```

Com Docker Desktop ativo, complete a validação:

```powershell
docker build --file Docker/Dockerfile --tag mcp-postgres:local .
docker compose --env-file .env --file Docker/docker-compose.yaml config
docker compose --env-file .env --file Docker/docker-compose.yaml up --build -d
docker compose --env-file .env --file Docker/docker-compose.yaml ps
Invoke-WebRequest http://localhost:8080/healthz
Invoke-WebRequest http://localhost:8080/readyz
```

Não considere a release liberada se os testes Go, o build, `git diff --check`,
health/readiness ou o contrato `initialize`/`tools/list` falharem. Em uma
máquina sem Docker, registre essa limitação e execute o build de imagem no CI.

### Checklist de setup e release

Antes do primeiro uso:

- [ ] `.env` foi criado de `.env.example` e não está sendo versionado;
- [ ] placeholders e credenciais de exemplo foram substituídos;
- [ ] `DATABASE_URL` aponta para o host correto dentro do ambiente de execução;
- [ ] `MCP_PUBLIC_BASE_URL` é a origem pública correta;
- [ ] `MCP_CITATION_SIGNING_KEY` possui pelo menos 32 caracteres;
- [ ] scopes, principals, connections e DML policies foram revisados;
- [ ] DDL/DML estão habilitados somente onde foram aprovados;
- [ ] masking e limites de rows/affected rows/script foram revisados;
- [ ] `/healthz` e `/readyz` respondem;
- [ ] `tools/list` autenticado mostra somente as tools esperadas.

Antes de uma carga SQL:

- [ ] o conteúdo foi enviado em `script`, não como caminho;
- [ ] a tool apropriada foi validada;
- [ ] `source_name` não contém segredo;
- [ ] o script não contém controle de transação, `SELECT`, `COPY`, `DO` ou `SET`;
- [ ] statement, byte e affected-row limits suportam a carga;
- [ ] a aprovação explícita está ativa;
- [ ] as contagens finais serão verificadas com `execute_sql`.

Antes de publicar:

- [ ] `VERSION` foi atualizado para o próximo SemVer;
- [ ] testes, vet, módulos, build e diff passaram;
- [ ] a imagem foi publicada com tag `vX.Y.Z`;
- [ ] o Swarm/Portainer aponta para a mesma tag;
- [ ] o proxy termina TLS e não publica PostgreSQL;
- [ ] volumes OAuth e dados foram preservados;
- [ ] o connector foi reconectado e suas tools foram redescobertas;
- [ ] logs não expõem tokens, DSN ou SQL sensível.
