# git-forge

A standalone, GitHub-compatible git forge in one Node process: git smart HTTP
(protocol v0 + v2), REST v3, GraphQL v4 and MCP — served from its own
in-memory store, no UI, no outside service, stdlib only.

Built for the make.fun bounty
[Build a GitHub-compatible git forge](https://make.fun/bounty/github-core-clone-01)
($500, ends Sat 21 Nov 2026 13:30 WIB).

## Run it

```sh
node --experimental-strip-types main.ts
```

`MAKEFUN_ENV_URL` must point at the harness env; the process binds an
ephemeral port, registers it, serves until the env says done, then exits 0.
Node 22.18+ (the platform runs `node /bot/main.ts`, which strips types on its
own). Nothing to install.

## Layout

| File | What it is |
| --- | --- |
| `main.ts` | Entry ↔ env lifecycle (bind, register, poll done, exit). Unchanged starter. |
| `app.ts` | HTTP server: health probe, REST v3, git smart HTTP (`info/refs`, `git-upload-pack`, `git-receive-pack`). |
| `store.ts` | In-memory store: SHA-1 git objects, packfile read/write (plain + delta), pkt-lines, trees/commits/tags, repos/issues/pull requests, squash merges. |
| `graphql.ts` | A small GraphQL executor for the documents `gh` and `github-mcp-server` send. |
| `mcp.ts` | MCP over streamable HTTP: `server/discover`, `initialize`, `tools/list`, `tools/call`. |

## Notes

- The authenticated user is the scenario viewer (`adaiversen`); every API call
  presents `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`, which the server
  validates against the same variable in its own environment.
- All server-made state is deterministic: fixed commit timestamps, counters
  from fixed starts, no clock reads in any stored value.
- Repositories are created through REST `POST /api/v3/user/repos` only; a
  smart-HTTP push cannot invent one. Pushes validate fast-forward per ref and
  refuse non-fast-forward updates with reason `non-fast-forward`.

## License

MIT — see `LICENSE`.
