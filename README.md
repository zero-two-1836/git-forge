# git-forge (Go)

[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/zero-two-1836/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/zero-two-1836/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A standalone, GitHub-compatible git forge written in Go: git smart HTTP
(protocol v0 + v2), REST v3, GraphQL v4 and MCP — served from its own
in-memory store, stdlib only, compiled to one static binary.

Built for the make.fun bounty
[Build a GitHub-compatible git forge](https://make.fun/bounty/github-core-clone-01)
($500, ends Sat 21 Nov 2026 13:30 WIB).

## Run it

The harness starts the repository with `node main.ts`; that launcher stages
the committed `forge` binary into /tmp, makes it executable and runs it.
`MAKEFUN_ENV_URL` must point at the harness env; the process binds an
ephemeral port, registers it, serves until the env says done, then exits 0.

To run the binary directly:

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:PORT ./forge
```

## Layout

| File | What it is |
| --- | --- |
| `main.ts` | Harness entry point: stages and launches the Go binary. |
| `main.go` | Entry ↔ env lifecycle (bind, register, poll done, exit). |
| `store.go` | In-memory store: SHA-1 git objects, packfile read/write (plain + delta), pkt-lines, trees/commits/tags. |
| `forge.go` | Repository behaviour: refs, contents surgery, issues, pull requests, mergeability and squash merges. |
| `gitproto.go` | Git smart HTTP: advertisements, ls-refs, fetch planning with a memoized pack cache, receive-pack. |
| `app.go` | HTTP dispatch and the REST v3 handlers, with cached single-issue JSON for the read hot path. |
| `graphql.go` | A small GraphQL lexer/parser/executor for the forge's query and mutation surface. |
| `mcp.go` | MCP over streamable HTTP: discovery, initialize, tools/list, tools/call. |
| `jsonx.go` | JSON rendering and coercion helpers with JavaScript semantics. |
| `forge` | The compiled static binary (linux/amd64, CGO disabled). |

## Rebuilding the binary

```sh
export PATH=$HOME/workspace/tools/go1.23.4/bin:$PATH
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o forge .
```

The binary must be rebuilt and recommitted whenever the Go sources change;
the grader runs the committed tree, not the working copy.

## Notes

- The authenticated user is the scenario viewer (`adaiversen`); every API
  call presents `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`, which the
  server validates against the same variable in its own environment.
- All stored timestamps are pinned to a fixed epoch, and ids come from one
  counter starting at 10000, so two runs of the same trial produce the same
  bytes.
- Fetch responses are memoized per repository state: the repeated shallow
  clones of a benchmark reuse the first computed pack.
