# MCP (AI assistants)

foxess-local serves the [Model Context Protocol](https://modelcontextprotocol.io)
at `http://<this machine>:18080/mcp` (streamable HTTP). An AI assistant
connected there can read the inverter and, with a token that allows it,
run the everyday commands.

It is a thin layer over the REST API: each tool is one API operation,
described from the API's own contract (`/api/v1/openapi.yaml`), and each
call is that API request made with your token. Everything the API
enforces applies unchanged:

- the token's **role** (read-only < standard < expert < installer);
- validation, and the **write budget** (at most 10 real writes per
  setting per hour);
- the **audit log**: every command appears under Activity with the
  token's name.

## Tools

| Tool | Does | Role |
|---|---|---|
| `get_state` | live power, battery, temperatures, settings, schedule | read-only |
| `get_history` | bucketed history and energy totals (1h … 30d) | read-only |
| `get_power_average` | mean power over the last 5 m … 1 h | read-only |
| `list_audit` | commands sent and cloud messages allowed or blocked | read-only |
| `get_forward_mode`, `get_maintenance` | what the FoxESS cloud may do | read-only |
| `get_compatibility`, `get_config`, `ready`, `get_update` | device support, server facts, health, releases | read-only |
| `set_work_mode` | self-use / feed-in | standard |
| `set_schedule` | the 8-group time table | standard |
| `set_meter_compensation` | meter offset | expert |
| `set_limits` | grid import / export limits | installer |

Deliberately **not** tools: tokens, updates, maintenance windows, cloud
forwarding, settings, raw register reads and the untested-writes grant.
Use the web UI for those.

## Connecting

Create a token for the assistant (**System → Access**).
Give it **read-only** unless you want it to change the work mode or
schedule (then **standard**). The token is optional: without one, the
assistant gets what visitors get (the dashboard data, while public
read-only access is on), and no commands.

Claude Code:

```bash
claude mcp add --transport http foxess http://foxess.local:18080/mcp \
  --header "Authorization: Bearer fl_…"
```

Other clients: point any MCP client that speaks streamable HTTP at the
URL, with the `Authorization: Bearer …` header. A client that only
speaks stdio can reach it through a bridge such as `mcp-remote`.

The endpoint is on your network only (like the web UI), refuses
requests from other websites (the `Origin` check), and runs no server-side
session: each request stands alone.
