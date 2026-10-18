# M0 spike 2 — waking a T3 thread (2026-10-16)

> **Superseded (PLAN v4):** eve no longer delivers into T3. Agents follow jobs with `eve wait`
> (client-side streaming); see docs/design.md §8. Kept for the record.

## Plan A: per-job T3 webhook task — not available on Quartz

`schedule_task {type: webhook}` bound to the requesting thread succeeded, but returned no
`webhookUrl`, and `list_scheduled_tasks` shows none. Per the tool contract, the URL only exists
when T3 Connect's managed tunnel is enabled, which it is not here. The task was deleted.

Enabling T3 Connect would make plan A work, at the cost of an internet-reachable webhook and one
scheduled task per job.

## Plan B: Eve calls T3's local MCP endpoint — viable, preferred

The T3 server on Quartz listens on `127.0.0.1:3773` and serves MCP at `/mcp`, protected by a
standard MCP OAuth authorization server:

- `POST /mcp` without a token → `401`, `WWW-Authenticate: Bearer
  resource_metadata="http://127.0.0.1:3773/.well-known/oauth-protected-resource/mcp"`.
- `/.well-known/oauth-protected-resource`: resource `http://127.0.0.1:3773/mcp`, scopes
  `orchestration:read`, `orchestration:operate`.
- `/.well-known/oauth-authorization-server`: `authorize` `/oauth/mcp/authorize`, `token`
  `/oauth/mcp/token`, dynamic client `registration` `/oauth/mcp/register`, `authorization_code`
  with PKCE S256, public clients (`token_endpoint_auth_methods_supported: none`). No refresh-token
  grant is advertised.

So Eve can:

1. `eve t3 login` (once, on Quartz): register as a public OAuth client, open the authorize URL
   for the user to approve `orchestration:operate`, store the access token 0600.
2. On a job event, call the MCP tool `t3_thread_send {threadId, message, clientRequestId}` with
   the result document. `clientRequestId = <job id>:<event>` makes redelivery idempotent on T3's
   side.
3. Agents pass their thread id: `eve submit … --notify-t3 <threadId>` (agents get it from
   `orchestrator_capabilities.parentThreadId`).

Benefits over plan A: loopback only, no tunnel, no per-job task to create and delete, and T3
queues the message if the thread is mid-turn.

Still to verify in M1, before relying on it:

- Token lifetime and what happens at expiry (no refresh grant advertised): if tokens are
  short-lived, Eve needs a re-login flow surfaced through `doctor` and the human webhook.
- That `t3_thread_send` to an idle thread starts a turn (expected from its contract:
  `mode=auto` "starts an idle thread"), and that a message sent while T3 is down is retried from
  Eve's outbox after T3 returns.
- Not done in this spike: the OAuth approval itself (needs the user in a browser) and the T3
  restart test (restarting T3 would end this session).
