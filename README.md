# jellyfin-mcp

Dependency-free MCP stdio server (Go stdlib only) exposing a read-only slice
of the Jellyfin API to Odysseus.

## Build

```sh
go build -o jellyfin-mcp .
```

## Env

- `JELLYFIN_URL` - base URL (default `http://127.0.0.1:7219`)
- `JELLYFIN_API_KEY` - API key, sent as the `X-MediaBrowser-Token` header
- `JELLYFIN_USER` - optional user name or id. Default is the first
  administrator, else the first user. API keys are not tied to a user, so
  `/Users/Me` cannot be used for the default.

## Tools

`search_media`, `list_libraries`, `item_details`, `latest_media`.
Search/latest/library payloads are projected to id, name, type, year and a
short overview to keep token use down.

## Hestia wiring

The binary is mounted read-only into the Odysseus container at
`/opt/host-mcp/jellyfin-mcp` (see `docker-compose.override.yml` in
`/mnt/media/llm/odysseus/src`) and registered in the `mcp_servers` table
(transport `stdio`). From inside the container the URL must be
`http://host.docker.internal:7219`.
