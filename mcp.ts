// mcp.ts — the Model Context Protocol endpoint over streamable HTTP: the modern
// 2026-07-28 era (server/discover, no handshake) and the legacy initialize
// handshake, tools/list and tools/call for the six github-mcp-server tools.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  ForgeStore,
  type Issue,
  type Pull,
  type Repo,
} from './store.ts';

const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Issue state filter: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

// The tools list, serialized once: two tools/list answers must be byte-identical.
const TOOLS_JSON = JSON.stringify({ tools: TOOLS });

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.dbId,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    has_issues: true,
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name, color: repo.labels.get(name)?.color ?? 'ededed' })),
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
  };
}

type ToolOutcome = { ok: true; value: unknown } | { ok: false; message: string };

function runTool(store: ForgeStore, name: string, args: Record<string, unknown>): ToolOutcome {
  const arg = (key: string): unknown => args[key];
  switch (name) {
    case 'create_repo': {
      const repoName = arg('name');
      if (typeof repoName !== 'string' || repoName === '') return { ok: false, message: 'create_repo requires a name' };
      const created = store.createRepo(repoName, arg('private') === true, typeof arg('description') === 'string' ? (arg('description') as string) : '');
      if (created === null) return { ok: false, message: 'Repository already exists' };
      return { ok: true, value: repoJson(created) };
    }
    case 'get_repo': {
      const owner = arg('owner');
      const repoName = arg('repo');
      if (typeof owner !== 'string' || typeof repoName !== 'string') return { ok: false, message: 'get_repo requires owner and repo' };
      const repo = store.getRepo(owner, repoName);
      if (repo === undefined) return { ok: false, message: 'Repository not found' };
      return { ok: true, value: repoJson(repo) };
    }
    case 'create_issue': {
      const owner = arg('owner');
      const repoName = arg('repo');
      const title = arg('title');
      if (typeof owner !== 'string' || typeof repoName !== 'string' || typeof title !== 'string' || title === '') {
        return { ok: false, message: 'create_issue requires owner, repo and title' };
      }
      const repo = store.getRepo(owner, repoName);
      if (repo === undefined) return { ok: false, message: 'Repository not found' };
      const created = store.createIssue(repo, title, typeof arg('body') === 'string' ? (arg('body') as string) : '', []);
      if (typeof created === 'object' && created !== null && 'status' in created) {
        return { ok: false, message: (created as { message: string }).message };
      }
      return { ok: true, value: issueJson(repo, created as Issue) };
    }
    case 'list_issues': {
      const owner = arg('owner');
      const repoName = arg('repo');
      if (typeof owner !== 'string' || typeof repoName !== 'string') return { ok: false, message: 'list_issues requires owner and repo' };
      const repo = store.getRepo(owner, repoName);
      if (repo === undefined) return { ok: false, message: 'Repository not found' };
      const state = typeof arg('state') === 'string' ? (arg('state') as string) : 'open';
      let issues = [...repo.issues.values()];
      if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
      issues.sort((a, b) => a.seq - b.seq);
      return { ok: true, value: { issues: issues.map((issue) => issueJson(repo, issue)) } };
    }
    case 'create_pull_request': {
      const owner = arg('owner');
      const repoName = arg('repo');
      const title = arg('title');
      const head = arg('head');
      const base = arg('base');
      if (typeof owner !== 'string' || typeof repoName !== 'string' || typeof title !== 'string' || typeof head !== 'string' || typeof base !== 'string') {
        return { ok: false, message: 'create_pull_request requires owner, repo, title, head and base' };
      }
      const repo = store.getRepo(owner, repoName);
      if (repo === undefined) return { ok: false, message: 'Repository not found' };
      const created = store.createPull(repo, title, typeof arg('body') === 'string' ? (arg('body') as string) : '', head, base);
      if (typeof created === 'object' && created !== null && 'status' in created) {
        return { ok: false, message: (created as { message: string }).message };
      }
      return { ok: true, value: pullJson(repo, created as Pull) };
    }
    case 'merge_pull_request': {
      const owner = arg('owner');
      const repoName = arg('repo');
      const pullNumber = arg('pull_number');
      if (typeof owner !== 'string' || typeof repoName !== 'string' || typeof pullNumber !== 'number') {
        return { ok: false, message: 'merge_pull_request requires owner, repo and pull_number' };
      }
      const repo = store.getRepo(owner, repoName);
      if (repo === undefined) return { ok: false, message: 'Repository not found' };
      const pull = repo.pulls.get(pullNumber);
      if (pull === undefined) return { ok: false, message: 'Pull request not found' };
      const merged = store.mergePull(repo, pull);
      if (typeof merged === 'object' && merged !== null && 'status' in merged) {
        return { ok: false, message: (merged as { message: string }).message };
      }
      return { ok: true, value: { sha: (merged as { sha: string }).sha, merged: true, message: `Merge pull request #${pull.number}` } };
    }
    default:
      return { ok: false, message: `Unknown tool: ${name}` };
  }
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = typeof body === 'string' ? body : `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

export async function handleMcp(
  req: IncomingMessage,
  res: ServerResponse,
  store: ForgeStore,
  isAuthed: (req: IncomingMessage) => boolean,
): Promise<void> {
  if (req.method === 'DELETE') {
    // A legacy session goodbye: nothing is held, so there is nothing to close.
    res.writeHead(204);
    res.end();
    return;
  }
  if (req.method !== 'POST') {
    sendJson(res, 405, { message: 'Method Not Allowed' });
    return;
  }
  const raw = await readBody(req);
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw.toString('utf8'));
  } catch {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'));
    return;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    sendJson(res, 400, rpcError(null, -32600, 'Invalid Request'));
    return;
  }
  const record = parsed as Record<string, unknown>;
  const id = record.id ?? null;
  const method = typeof record.method === 'string' ? record.method : '';
  const params = (record.params as Record<string, unknown> | undefined) ?? {};

  // Notifications carry no id and get no JSON-RPC answer.
  if (record.id === undefined) {
    if (!isAuthed(req)) {
      sendJson(res, 401, rpcError(null, -32001, 'Unauthorized'));
      return;
    }
    res.writeHead(202);
    res.end();
    return;
  }

  if (!isAuthed(req)) {
    sendJson(res, 401, rpcError(id, -32001, 'Unauthorized'));
    return;
  }

  if (method === 'server/discover') {
    sendJson(res, 200, rpcResult(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      instructions: 'A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP.',
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    }));
    return;
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const negotiated = SUPPORTED_VERSIONS.includes(requested) ? requested : '2026-07-28';
    sendJson(res, 200, rpcResult(id, {
      protocolVersion: negotiated,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    }));
    return;
  }

  if (method === 'ping') {
    sendJson(res, 200, rpcResult(id, {}));
    return;
  }

  if (method === 'tools/list') {
    // The tools array is a frozen constant: byte-identical across calls.
    sendJson(res, 200, rpcResult(id, JSON.parse(TOOLS_JSON) as Record<string, unknown>));
    return;
  }

  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments as Record<string, unknown> | undefined) ?? {};
    const tool = TOOLS.find((entry) => entry.name === toolName);
    if (tool === undefined) {
      sendJson(res, 200, rpcError(id, -32601, `Method not found: ${toolName}`));
      return;
    }
    const required = (tool.inputSchema.required as string[] | undefined) ?? [];
    const missing = required.filter((key) => args[key] === undefined);
    if (missing.length > 0) {
      const text = JSON.stringify({ error: `Missing required arguments: ${missing.join(', ')}` });
      sendJson(res, 200, rpcResult(id, { content: [{ type: 'text', text }], structuredContent: { error: `Missing required arguments: ${missing.join(', ')}` }, isError: true }));
      return;
    }
    const outcome = runTool(store, toolName, args);
    if (!outcome.ok) {
      const text = JSON.stringify({ error: outcome.message });
      sendJson(res, 200, rpcResult(id, { content: [{ type: 'text', text }], structuredContent: { error: outcome.message }, isError: true }));
      return;
    }
    const text = JSON.stringify(outcome.value);
    sendJson(res, 200, rpcResult(id, { content: [{ type: 'text', text }], structuredContent: outcome.value, isError: false }));
    return;
  }

  sendJson(res, 200, rpcError(id, -32601, `Method not found: ${method}`));
}
