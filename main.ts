// main.ts — the entrypoint for git-forge.
//
// This file owns the entry <-> environment lifecycle, and nothing else. The
// forge itself lives in the other modules:
//
//   - app.ts    builds the HTTP server: a /healthz probe, REST v3, git smart
//               HTTP (info/refs, git-upload-pack, git-receive-pack, v0 and v2),
//               GraphQL v4 (graphql.ts) and MCP over streamable HTTP (mcp.ts),
//               all served from the single in-memory ForgeStore in store.ts.
//   - store.ts  the store: SHA-1 git objects, packfile read/write, pkt-lines,
//               trees/commits/tags, repos/issues/pull requests.
//   - graphql.ts / mcp.ts  the two protocol endpoints app.ts dispatches to.
//
// The harness (env) drives the process through four steps:
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 inside a
//      trial container). Without it there is nothing to talk to, so the
//      process exits 1 at once.
//   2. The server binds an ephemeral port: listen(0) lets the kernel pick a
//      free one. Port 8080 is never bound — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the
//      address the request came from plus N as this entry's base URL and
//      dials it from then on. The first register wins; a 409 means another
//      entry already registered, and this process keeps waiting like any
//      other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with
//      a short exponential backoff; the process never exits before the env
//      says done. Then the server is closed and the process exits 0, and the
//      harness stops the env, which writes the export the scorer reads.
//
// Only stdlib is used; the program makes no model calls and reads no clock
// for anything it stores.

import { createApp } from './app.ts';
import type { Server } from 'node:http';

/** How long one register or done request may run before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** First wait between retries, longest wait between retries, and the cap on the exponent. */
const RETRY_FIRST_MS = 250;
const RETRY_MAX_MS = 2_000;
const RETRY_MAX_EXPONENT = 8;

/** Read MAKEFUN_ENV_URL, dropping any trailing slashes; exit 1 when it is unset. */
function resolveEnvUrl(): string {
  const url = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
  if (url === '') {
    process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
    process.exit(1);
  }
  return url;
}

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** Wait after `attempt` failed requests: exponential from RETRY_FIRST_MS, capped at RETRY_MAX_MS. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_FIRST_MS * 2 ** Math.min(attempt, RETRY_MAX_EXPONENT));
}

/**
 * Boot the forge server on an ephemeral port and return it with the port it
 * bound. createApp() only builds the server — nothing listens until here.
 */
async function bootServer(): Promise<{ server: Server; port: number }> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  return { server, port: address.port };
}

/**
 * POST the bound port to the env until it accepts it. 200 is the normal
 * answer; 409 means a first register already won, which is not this
 * process's to fix. 400 and 405 are contract errors and end the program; a
 * network error or any other status is retried, since the env may still be
 * starting.
 */
async function register(envUrl: string, port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done": true}; never return before that. */
async function waitForDone(envUrl: string): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/**
 * The whole life of the process, in order:
 *
 *   1. resolve the env URL (exits 1 when unset — there is no env to talk to);
 *   2. boot the forge server (app.ts: health probe, REST v3, git smart HTTP,
 *      GraphQL, MCP, over the one ForgeStore);
 *   3. register the bound port with the env;
 *   4. poll until the env says the trial is done;
 *   5. close the server and return, so the runner can exit 0.
 */
async function start(): Promise<void> {
  const envUrl = resolveEnvUrl();

  const { server, port } = await bootServer();
  process.stdout.write(`[entry] listening on port ${port}\n`);

  await register(envUrl, port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone(envUrl);
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await start();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
