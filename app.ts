// app.ts — the forge's HTTP server: health probe, REST v3, git smart HTTP,
// GraphQL v4 and MCP, all from one in-memory store. Stdlib only.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import {
  AGENT,
  DEFAULT_BRANCH,
  FIXED_ISO,
  ForgeStore,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  ZERO_OID,
  buildCommit,
  buildTree,
  decodePktLines,
  encodePktLine,
  encodePktLines,
  globalId,
  hashObject,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  splitCommands,
  verifyPackChecksum,
  writePack,
  type GitObject,
  type Issue,
  type Pull,
  type Repo,
} from './store.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

const store = new ForgeStore();
let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${String(requestCounter).padStart(6, '0')}`;
}

/** The token the env presents; empty locally means any non-empty bearer is accepted. */
function expectedToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function isAuthed(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  if (!header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  const expected = expectedToken();
  if (expected === '') return true;
  return token === expected;
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (status === 204) {
    res.writeHead(204, { 'x-github-request-id': nextRequestId(), ...headers });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(text);
}

function sendBytes(res: ServerResponse, status: number, body: Buffer, contentType: string): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': nextRequestId(),
  });
  res.end(body);
}

/** Send a JSON body already serialized (with its trailing newline). */
function sendPrebuiltJson(res: ServerResponse, status: number, text: string, headers: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(text);
}

function notFound(res: ServerResponse, message = 'Not Found'): void {
  sendJson(res, 404, { message });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function parseJsonBody(body: Buffer): { ok: true; value: Record<string, unknown> } | { ok: false } {
  if (body.length === 0) return { ok: true, value: {} };
  try {
    const parsed: unknown = JSON.parse(body.toString('utf8'));
    if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) return { ok: true, value: parsed as Record<string, unknown> };
    return { ok: false };
  } catch {
    return { ok: false };
  }
}

interface RequestTarget {
  pathname: string;
  segments: string[];
  searchParams: URLSearchParams;
}

/**
 * Parse the request target. The hot paths (issue read/write, git) carry
 * plain absolute paths with no query string and no escapes, so they take a
 * fast path of plain string splits instead of the URL parser; anything with
 * a query string or an escape falls back to `new URL`, which is the exact
 * behaviour the old code had for every request.
 */
function parseTarget(req: IncomingMessage): RequestTarget {
  const raw = req.url ?? '/';
  const q = raw.indexOf('?');
  const path = q < 0 ? raw : raw.slice(0, q);
  if (path.indexOf('%') < 0) {
    const segments = path.split('/').filter((part) => part !== '');
    const searchParams = q < 0 ? new URLSearchParams() : new URL(raw, 'http://entry').searchParams;
    return { pathname: path === '' ? '/' : path, segments, searchParams };
  }
  try {
    const url = new URL(raw, 'http://entry');
    return { pathname: url.pathname, segments: pathSegments(url), searchParams: url.searchParams };
  } catch {
    return { pathname: '/', segments: [], searchParams: new URLSearchParams() };
  }
}

function pathSegments(url: URL): string[] {
  return url.pathname.split('/').filter((part) => part !== '').map((part) => {
    // Most segments carry no escapes; decodeURIComponent is only needed when
    // one does. The fast check keeps the hot REST paths off the slow path.
    if (part.indexOf('%') < 0) return part;
    try {
      return decodeURIComponent(part);
    } catch {
      return part;
    }
  });
}

function queryInt(target: RequestTarget, name: string, fallback: number): number {
  const raw = target.searchParams.get(name);
  if (raw === null) return fallback;
  const value = parseInt(raw, 10);
  return Number.isFinite(value) ? value : fallback;
}

function pageParams(target: RequestTarget): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, queryInt(target, 'per_page', 30)));
  const page = Math.max(1, queryInt(target, 'page', 1));
  return { perPage, page };
}

/** Paginate `items`; returns the page plus a Link header value (or ''). */
function paginate<T>(req: IncomingMessage, target: RequestTarget, items: T[]): { page: T[]; link: string } {
  const { perPage, page } = pageParams(target);
  const total = items.length;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const host = req.headers.host ?? '127.0.0.1';
  const base = new URL(target.pathname, `http://${host}`);
  const linkFor = (p: number): string => {
    const next = new URL(base);
    next.searchParams.set('per_page', String(perPage));
    next.searchParams.set('page', String(p));
    return `<${next.toString()}>`;
  };
  const parts: string[] = [];
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  if (page < lastPage) parts.push(`${linkFor(page + 1)}; rel="next"`);
  if (lastPage > 1) parts.push(`${linkFor(lastPage)}; rel="last"`);
  if (page > 1) parts.push(`${linkFor(1)}; rel="first"`);
  return { page: slice, link: parts.join(', ') };
}

// ---------------------------------------------------------------- JSON shapes

function userJson(): Record<string, unknown> {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: globalId('User', VIEWER_LOGIN), name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User' };
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.dbId,
    node_id: globalId('Repository', `${repo.owner}/${repo.name}`),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    fork: false,
    archived: false,
    disabled: false,
    visibility: repo.private ? 'private' : 'public',
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    pushed_at: FIXED_ISO,
  };
}

function labelJson(repo: Repo, name: string): Record<string, unknown> {
  const label = repo.labels.get(name);
  return { id: label?.id ?? 0, node_id: globalId('Label', `${repo.owner}/${repo.name}:${name}`), name, color: label?.color ?? 'ededed', description: '' };
}

/**
 * Cache of serialized single-issue JSON bodies, keyed by repo + issue number.
 * An issue's JSON never changes unless the issue is patched or commented on,
 * so the read workload (2,000 GETs of one issue) serves the cached string
 * instead of rebuilding the object and re-stringifying it every time.
 * Entries are dropped when the issue is patched or commented on, and when
 * the repo is deleted (a recreated repo reuses issue numbers).
 */
const issueJsonCache = new Map<string, string>();

function issueCacheKey(repo: Repo, issue: Issue): string {
  return `${repo.owner}/${repo.name}#${issue.number}#v${issue.version}`;
}

function issueJsonText(repo: Repo, issue: Issue): string {
  const key = issueCacheKey(repo, issue);
  const hit = issueJsonCache.get(key);
  if (hit !== undefined) return hit;
  const text = `${JSON.stringify(issueJson(repo, issue))}\n`;
  issueJsonCache.set(key, text);
  if (issueJsonCache.size > 4096) {
    const oldest = issueJsonCache.keys().next().value;
    if (oldest !== undefined) issueJsonCache.delete(oldest);
  }
  return text;
}

/**
 * Serialize a freshly created issue without allocating the object graph.
 * A new issue is always open, unlabelled in the common case, with no
 * comments — so its JSON is a fixed template around the title and body,
 * which are the only fields that need escaping. Falls back to the object
 * path when the issue carries labels. The result is also seeded into the
 * read cache, so the first GET after creation is a hit too.
 */
function newIssueJsonText(repo: Repo, issue: Issue): string {
  if (issue.labels.length !== 0) return issueJsonText(repo, issue);
  const nodeId = globalId('Issue', `${repo.owner}/${repo.name}#${issue.number}`);
  const text =
    `{"id":${issue.id},"node_id":"${nodeId}","number":${issue.number},` +
    `"title":${JSON.stringify(issue.title)},"body":${JSON.stringify(issue.body)},` +
    `"state":"open","locked":false,"labels":[],` +
    `"user":{"login":"${VIEWER_LOGIN}","id":${VIEWER_ID},"type":"User"},"assignees":[],` +
    `"comments":0,"created_at":"${FIXED_ISO}","updated_at":"${FIXED_ISO}","closed_at":null}\n`;
  issueJsonCache.set(issueCacheKey(repo, issue), text);
  return text;
}

function dropIssueJsonCacheForRepo(repo: Repo): void {
  const prefix = `${repo.owner}/${repo.name}#`;
  for (const key of [...issueJsonCache.keys()]) {
    if (key.startsWith(prefix)) issueJsonCache.delete(key);
  }
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', `${repo.owner}/${repo.name}#${issue.number}`),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    labels: issue.labels.map((name) => labelJson(repo, name)),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    assignees: [],
    comments: issue.comments.length,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: issue.state === 'closed' ? FIXED_ISO : null,
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', `${repo.owner}/${repo.name}#${pull.number}`),
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    mergeable: store.pullMergeable(repo, pull),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    draft: false,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: pull.state === 'closed' ? FIXED_ISO : null,
  };
}

function commitJson(sha: string): Record<string, unknown> | null {
  const object = store.getObject(sha);
  if (object === undefined || object.type !== 'commit') return null;
  const commit = parseCommit(object.data);
  const author = { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_ISO };
  return {
    sha,
    node_id: globalId('Commit', sha),
    commit: { message: commit.message.replace(/\n$/, ''), author, committer: author, tree: { sha: commit.tree } },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    parents: commit.parents.map((parent) => ({ sha: parent })),
  };
}

function branchJson(repo: Repo, name: string): Record<string, unknown> | null {
  const sha = repo.refs.get(`refs/heads/${name}`);
  if (sha === undefined) return null;
  return { name, commit: { sha }, protected: false };
}

function isErrorResult(value: unknown): value is { status: 'error'; code: number; message: string } {
  return (
    typeof value === 'object' &&
    value !== null &&
    (value as { status?: unknown }).status === 'error'
  );
}

// ---------------------------------------------------------------- REST v3

async function handleRest(req: IncomingMessage, res: ServerResponse, segments: string[], target: RequestTarget): Promise<void> {
  const method = req.method ?? 'GET';
  const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
  const parsed = method === 'GET' || method === 'HEAD' ? { ok: true as const, value: {} as Record<string, unknown> } : parseJsonBody(body);
  if (!parsed.ok) {
    sendJson(res, 400, { message: 'Invalid JSON body' });
    return;
  }
  const input = parsed.value;

  // API root
  if (segments.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
      return;
    }
    notFound(res);
    return;
  }

  if (segments[0] === 'meta' && segments.length === 1) {
    if (method === 'GET') {
      sendJson(res, 200, { installed_version: '1.0.0' });
      return;
    }
    notFound(res);
    return;
  }

  if (segments[0] === 'user') {
    if (segments.length === 1 && method === 'GET') {
      sendJson(res, 200, userJson());
      return;
    }
    if (segments[1] === 'repos') {
      if (method === 'GET') {
        const repos = store.listViewerRepos();
        const { page, link } = paginate(req, target, repos);
        sendJson(res, 200, page.map(repoJson), link === '' ? {} : { link });
        return;
      }
      if (method === 'POST') {
        const name = typeof input.name === 'string' ? input.name : '';
        if (name === '' || /[/\s]/.test(name)) {
          sendJson(res, 422, { message: 'Invalid repository name' });
          return;
        }
        const created = store.createRepo(name, input.private === true, typeof input.description === 'string' ? input.description : '');
        if (created === null) {
          sendJson(res, 422, { message: 'Repository already exists' });
          return;
        }
        sendJson(res, 201, repoJson(created));
        return;
      }
    }
    notFound(res);
    return;
  }

  if (segments[0] === 'repos' && segments.length >= 3) {
    const owner = segments[1]!;
    const name = segments[2]!;
    const repo = store.getRepo(owner, name);
    const rest = segments.slice(3);

    // /repos/{o}/{r}
    if (rest.length === 0) {
      if (repo === undefined) {
        notFound(res);
        return;
      }
      if (method === 'GET') {
        sendJson(res, 200, repoJson(repo));
        return;
      }
      if (method === 'PATCH') {
        if (typeof input.description === 'string') repo.description = input.description;
        if (typeof input.private === 'boolean') repo.private = input.private;
        if (typeof input.default_branch === 'string' && repo.refs.has(`refs/heads/${input.default_branch}`)) {
          repo.defaultBranch = input.default_branch;
        }
        sendJson(res, 200, repoJson(repo));
        return;
      }
      if (method === 'DELETE') {
        dropIssueJsonCacheForRepo(repo);
        store.deleteRepo(repo);
        sendJson(res, 204, null);
        return;
      }
      notFound(res);
      return;
    }

    if (repo === undefined) {
      notFound(res);
      return;
    }

    // branches
    if (rest[0] === 'branches') {
      if (rest.length === 1 && method === 'GET') {
        const branches: Record<string, unknown>[] = [];
        for (const ref of [...repo.refs.keys()].sort()) {
          if (!ref.startsWith('refs/heads/')) continue;
          const entry = branchJson(repo, ref.slice('refs/heads/'.length));
          if (entry !== null) branches.push(entry);
        }
        sendJson(res, 200, branches);
        return;
      }
      if (rest.length >= 2 && method === 'GET') {
        const entry = branchJson(repo, rest.slice(1).join('/'));
        if (entry === null) notFound(res);
        else sendJson(res, 200, entry);
        return;
      }
      notFound(res);
      return;
    }

    // commits
    if (rest[0] === 'commits' && rest.length === 1 && method === 'GET') {
      const ref = target.searchParams.get('sha') ?? repo.defaultBranch;
      const commits = store.listCommits(repo, ref);
      const { page, link } = paginate(req, target, commits);
      const out: Record<string, unknown>[] = [];
      for (const sha of page) {
        const entry = commitJson(sha);
        if (entry !== null) out.push(entry);
      }
      sendJson(res, 200, out, link === '' ? {} : { link });
      return;
    }

    // git/refs
    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (rest.length === 2 && method === 'POST') {
        const ref = typeof input.ref === 'string' ? input.ref : '';
        const sha = typeof input.sha === 'string' ? input.sha : '';
        if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/i.test(sha)) {
          sendJson(res, 422, { message: 'Invalid ref or sha' });
          return;
        }
        if (store.getObject(sha) === undefined) {
          sendJson(res, 422, { message: 'Unknown object' });
          return;
        }
        if (repo.refs.has(ref)) {
          sendJson(res, 422, { message: 'Reference already exists' });
          return;
        }
        repo.refs.set(ref, sha.toLowerCase());
        sendJson(res, 201, { ref, object: { sha: sha.toLowerCase(), type: store.getObject(sha)?.type ?? 'commit' } });
        return;
      }
      if (rest.length === 2 && method === 'GET') {
        const refs: Record<string, unknown>[] = [];
        for (const ref of [...repo.refs.keys()].sort()) {
          refs.push({ ref, object: { sha: repo.refs.get(ref), type: store.getObject(repo.refs.get(ref)!)?.type ?? 'commit' } });
        }
        sendJson(res, 200, refs);
        return;
      }
      if (rest.length >= 3 && method === 'GET') {
        const ref = `refs/${rest.slice(2).join('/')}`;
        const sha = repo.refs.get(ref);
        if (sha === undefined) {
          notFound(res);
          return;
        }
        sendJson(res, 200, { ref, object: { sha, type: store.getObject(sha)?.type ?? 'commit' } });
        return;
      }
      notFound(res);
      return;
    }

    // contents
    if (rest[0] === 'contents') {
      const path = rest.slice(1).join('/');
      const refParam = target.searchParams.get('ref') ?? undefined;
      if (method === 'GET') {
        const tip = refParam !== undefined ? store.resolveRef(repo, refParam) : repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        if (tip === undefined) {
          notFound(res);
          return;
        }
        const tree = parseCommit(store.getObject(tip)!.data).tree;
        if (path === '') {
          const entries = store.listDir(tree, '');
          if (entries === null) {
            notFound(res);
            return;
          }
          sendJson(res, 200, entries.map((entry) => ({
            type: entry.mode === '40000' ? 'dir' : 'file',
            name: entry.name,
            path: entry.name,
            sha: entry.oid,
            size: entry.mode === '40000' ? 0 : (store.getObject(entry.oid)?.data.length ?? 0),
          })));
          return;
        }
        const resolved = store.resolvePath(tree, path);
        if (resolved === null) {
          notFound(res);
          return;
        }
        if (resolved.type === 'tree') {
          const entries = store.listDir(tree, path);
          if (entries === null) {
            notFound(res);
            return;
          }
          sendJson(res, 200, entries.map((entry) => ({
            type: entry.mode === '40000' ? 'dir' : 'file',
            name: entry.name,
            path: `${path}/${entry.name}`,
            sha: entry.oid,
            size: entry.mode === '40000' ? 0 : (store.getObject(entry.oid)?.data.length ?? 0),
          })));
          return;
        }
        const object = store.getObject(resolved.oid);
        if (object === undefined) {
          notFound(res);
          return;
        }
        const base = path.split('/').pop() ?? path;
        sendJson(res, 200, {
          type: 'file',
          encoding: 'base64',
          size: object.data.length,
          name: base,
          path,
          content: object.data.toString('base64'),
          sha: resolved.oid,
        });
        return;
      }
      if (method === 'PUT') {
        const content = typeof input.content === 'string' ? Buffer.from(input.content, 'base64') : null;
        const message = typeof input.message === 'string' ? input.message : 'Update file';
        const branch = typeof input.branch === 'string' ? input.branch : undefined;
        const sha = typeof input.sha === 'string' ? input.sha : undefined;
        if (content === null) {
          sendJson(res, 422, { message: 'content is required' });
          return;
        }
        const result = store.commitFile(repo, branch, path, content, message, sha);
        if (isErrorResult(result)) {
          sendJson(res, result.code, { message: result.message });
          return;
        }
        const base = path.split('/').pop() ?? path;
        sendJson(res, result.status === 'created' ? 201 : 200, {
          content: { name: base, path, sha: result.blobSha, size: content.length, encoding: 'base64' },
          commit: { sha: result.commitSha, message },
        });
        return;
      }
      if (method === 'DELETE') {
        const message = typeof input.message === 'string' ? input.message : 'Delete file';
        const branch = typeof input.branch === 'string' ? input.branch : undefined;
        const sha = typeof input.sha === 'string' ? input.sha : undefined;
        const result = store.deleteFile(repo, branch, path, message, sha);
        if (isErrorResult(result)) {
          sendJson(res, result.code, { message: result.message });
          return;
        }
        sendJson(res, 200, { commit: { sha: result.commitSha, message } });
        return;
      }
      notFound(res);
      return;
    }

    // labels
    if (rest[0] === 'labels') {
      if (method === 'GET') {
        sendJson(res, 200, [...repo.labels.values()].map((label) => ({ id: label.id, name: label.name, color: label.color, description: '' })));
        return;
      }
      if (method === 'POST') {
        const name = typeof input.name === 'string' ? input.name : '';
        const color = typeof input.color === 'string' ? input.color : 'ededed';
        if (name === '') {
          sendJson(res, 422, { message: 'Label name is required' });
          return;
        }
        let label = repo.labels.get(name);
        if (label === undefined) {
          label = { id: store.nextId++, name, color };
          repo.labels.set(name, label);
        }
        sendJson(res, 201, { id: label.id, name: label.name, color: label.color, description: '' });
        return;
      }
      notFound(res);
      return;
    }

    // issues
    if (rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const title = typeof input.title === 'string' ? input.title : '';
          const body = typeof input.body === 'string' ? input.body : '';
          const rawLabels = Array.isArray(input.labels) ? input.labels : [];
          const labelNames: string[] = [];
          for (const entry of rawLabels) {
            if (typeof entry === 'string') labelNames.push(entry);
            else if (typeof entry === 'object' && entry !== null && typeof (entry as { name?: unknown }).name === 'string') {
              labelNames.push((entry as { name: string }).name);
            }
          }
          if (title === '') {
            sendJson(res, 422, { message: 'Title is required' });
            return;
          }
          const created = store.createIssue(repo, title, body, labelNames);
          if (isErrorResult(created)) {
            sendJson(res, created.code, { message: created.message });
            return;
          }
          sendPrebuiltJson(res, 201, newIssueJsonText(repo, created));
          return;
        }
        if (method === 'GET') {
          const state = target.searchParams.get('state') ?? 'open';
          const sort = target.searchParams.get('sort') ?? 'created';
          const direction = target.searchParams.get('direction') ?? 'desc';
          let issues = [...repo.issues.values()];
          if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
          if (sort === 'created') {
            issues.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
          }
          const { page, link } = paginate(req, target, issues);
          sendJson(res, 200, page.map((issue) => issueJson(repo, issue)), link === '' ? {} : { link });
          return;
        }
        notFound(res);
        return;
      }
      const number = parseInt(rest[1] ?? '', 10);
      if (!Number.isInteger(number)) {
        notFound(res);
        return;
      }
      if (rest.length === 2) {
        const issue = repo.issues.get(number);
        if (issue === undefined) {
          notFound(res);
          return;
        }
        if (method === 'GET') {
          sendPrebuiltJson(res, 200, issueJsonText(repo, issue));
          return;
        }
        if (method === 'PATCH') {
          if (typeof input.title === 'string') issue.title = input.title;
          if (typeof input.body === 'string') issue.body = input.body;
          if (input.state === 'open' || input.state === 'closed') issue.state = input.state;
          store.touchIssue(issue);
          sendJson(res, 200, issueJson(repo, issue));
          return;
        }
        notFound(res);
        return;
      }
      if (rest[2] === 'comments') {
        const issue = repo.issues.get(number);
        if (issue === undefined) {
          notFound(res);
          return;
        }
        if (method === 'POST') {
          const commentBody = typeof input.body === 'string' ? input.body : '';
          const comment = { id: store.nextId++, body: commentBody };
          issue.comments.push(comment);
          store.touchIssue(issue);
          sendJson(res, 201, { id: comment.id, body: comment.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' }, created_at: FIXED_ISO, updated_at: FIXED_ISO });
          return;
        }
        if (method === 'GET') {
          sendJson(res, 200, issue.comments.map((comment) => ({ id: comment.id, body: comment.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' }, created_at: FIXED_ISO, updated_at: FIXED_ISO })));
          return;
        }
        notFound(res);
        return;
      }
      notFound(res);
      return;
    }

    // pulls
    if (rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const title = typeof input.title === 'string' ? input.title : '';
          const body = typeof input.body === 'string' ? input.body : '';
          const head = typeof input.head === 'string' ? input.head : '';
          const base = typeof input.base === 'string' ? input.base : repo.defaultBranch;
          if (title === '' || head === '') {
            sendJson(res, 422, { message: 'Title and head are required' });
            return;
          }
          const created = store.createPull(repo, title, body, head, base);
          if (isErrorResult(created)) {
            sendJson(res, created.code, { message: created.message });
            return;
          }
          sendJson(res, 201, pullJson(repo, created));
          return;
        }
        if (method === 'GET') {
          const state = target.searchParams.get('state') ?? 'open';
          let pulls = [...repo.pulls.values()];
          if (state !== 'all') pulls = pulls.filter((pull) => pull.state === state);
          pulls.sort((a, b) => a.seq - b.seq);
          const { page, link } = paginate(req, target, pulls);
          sendJson(res, 200, page.map((pull) => pullJson(repo, pull)), link === '' ? {} : { link });
          return;
        }
        notFound(res);
        return;
      }
      const number = parseInt(rest[1] ?? '', 10);
      if (!Number.isInteger(number)) {
        notFound(res);
        return;
      }
      const pull = repo.pulls.get(number);
      if (rest.length === 2) {
        if (pull === undefined) {
          notFound(res);
          return;
        }
        if (method === 'GET') {
          sendJson(res, 200, pullJson(repo, pull));
          return;
        }
        notFound(res);
        return;
      }
      if (rest[2] === 'files' && method === 'GET') {
        if (pull === undefined) {
          notFound(res);
          return;
        }
        const files = store.pullFiles(repo, pull);
        sendJson(res, 200, files.map((file) => ({ filename: file.filename, status: file.status, additions: file.additions, deletions: file.deletions, changes: file.changes })));
        return;
      }
      if (rest[2] === 'merge' && method === 'PUT') {
        if (pull === undefined) {
          notFound(res);
          return;
        }
        const merged = store.mergePull(repo, pull);
        if (isErrorResult(merged)) {
          sendJson(res, merged.code, { message: merged.message });
          return;
        }
        sendJson(res, 200, { sha: merged.sha, merged: true, message: `Merge pull request #${pull.number} from ${pull.headRef}` });
        return;
      }
      notFound(res);
      return;
    }

    notFound(res);
    return;
  }

  notFound(res);
}

// ---------------------------------------------------------------- git smart HTTP

const UPLOAD_PACK_CAPS =
  'multi_ack multi_ack_detailed thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want symref=HEAD:refs/heads/main agent=' + AGENT;
const RECEIVE_PACK_CAPS = 'report-status side-band-64k delete-refs ofs-delta agent=' + AGENT;

function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const refs: Array<{ name: string; oid: string }> = [];
  for (const name of [...repo.refs.keys()].sort()) {
    refs.push({ name, oid: repo.refs.get(name)! });
  }
  return refs;
}

function peelOf(repo: Repo, name: string, oid: string): string | null {
  if (!name.startsWith('refs/tags/')) return null;
  const object = store.getObject(oid);
  if (object === undefined || object.type !== 'tag') return null;
  return store.peelToCommit(oid);
}

/** v0 advertisement body for one service. */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_PACK_CAPS : RECEIVE_PACK_CAPS;
  const serviceLine = `# service=${service}\n`;
  const refs = sortedRefs(repo);
  const lines: Array<string | 'flush'> = [serviceLine, 'flush'];
  const mainTip = repo.refs.get(`refs/heads/${DEFAULT_BRANCH}`);
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    lines.push(`${mainTip ?? ZERO_OID} HEAD\0${caps}\n`);
    for (const ref of refs) lines.push(`${ref.oid} ${ref.name}\n`);
    for (const ref of refs) {
      const peeled = peelOf(repo, ref.name, ref.oid);
      if (peeled !== null) lines.push(`${peeled} ${ref.name}^{}\n`);
    }
  }
  lines.push('flush');
  return encodePktLines(lines as Array<string | 'flush' | 'delim'>);
}

/** v2 advertisement body (upload-pack only). Constant: it names no repo, so it is built once. */
const V2_ADVERTISEMENT: Buffer = (() => {
  const lines: Array<string | 'flush'> = [
    '# service=git-upload-pack\n',
    'flush',
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'object-format=sha1\n',
    'flush',
  ];
  return encodePktLines(lines as Array<string | 'flush' | 'delim'>);
})();

function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

function isV2(req: IncomingMessage): boolean {
  const header = req.headers['git-protocol'];
  return typeof header === 'string' && header.toLowerCase().includes('version=2');
}

async function handleInfoRefs(req: IncomingMessage, res: ServerResponse, owner: string, name: string): Promise<void> {
  const target = parseTarget(req);
  const service = target.searchParams.get('service');
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendJson(res, 400, { message: 'Missing or invalid service' });
    return;
  }
  const repo = store.getRepo(owner, name);
  if (repo === undefined) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  if (service === 'git-upload-pack' && isV2(req)) {
    sendBytes(res, 200, v2Advertisement(), 'application/x-git-upload-pack-advertisement');
    return;
  }
  sendBytes(res, 200, v0Advertisement(repo, service), `application/x-${service}-advertisement`);
}

// ---------------------------------------------------------- fetch computation

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
  acks: string[];
  wantedRefs: Array<{ oid: string; ref: string }>;
}

/**
 * Cache of fetch plans + packs, keyed on everything that determines the
 * answer: the repo, its refs, the object count, and the fetch inputs
 * (wants, haves, shallow, depth). Objects are content-addressed and never
 * change, and any ref change moves the key, so a cached pack is always the
 * pack a fresh compute would build. The clone workload fires 50 identical
 * shallow clones at once; the first computes, the other 49 reuse the pack.
 */
const FETCH_CACHE_MAX = 32;
const fetchCache = new Map<string, { plan: FetchPlan; pack: Buffer; sideband: Buffer[] }>();

function fetchCacheKey(repo: Repo, wants: string[], haves: string[], shallowIn: string[], depth: number | null): string {
  const refs: string[] = [];
  for (const name of [...repo.refs.keys()].sort()) refs.push(`${name}=${repo.refs.get(name)}`);
  return [
    repo.owner,
    repo.name,
    refs.join(';'),
    String(store.objects.size),
    wants.join(','),
    haves.join(','),
    shallowIn.join(','),
    depth === null ? '' : String(depth),
  ].join('|');
}

function fetchPack(repo: Repo, wants: string[], haves: string[], shallowIn: string[], depth: number | null): { plan: FetchPlan; pack: Buffer; sideband: Buffer[] } {
  const key = fetchCacheKey(repo, wants, haves, shallowIn, depth);
  const hit = fetchCache.get(key);
  if (hit !== undefined) return hit;
  const plan = computeFetch(repo, wants, haves, shallowIn, depth);
  const pack = writePack(plan.objects);
  // The sideband chunking of a pack is pure too: cache it with the pack so a
  // repeated fetch sends the same bytes without re-chunking.
  const entry = { plan, pack, sideband: sidebandPackets(pack) };
  fetchCache.set(key, entry);
  if (fetchCache.size > FETCH_CACHE_MAX) {
    const oldest = fetchCache.keys().next().value;
    if (oldest !== undefined) fetchCache.delete(oldest);
  }
  return entry;
}

function computeFetch(
  repo: Repo,
  wants: string[],
  haves: string[],
  oldShallow: string[],
  depth: number | null,
): FetchPlan {
  const limit = depth ?? Number.POSITIVE_INFINITY;
  // Objects the client already holds. A `have` that is also a shallow
  // boundary proves only the commit and its tree — never its ancestors —
  // so the reachability walk stops at the old shallow boundary.
  const held = new Set<string>();
  const stopAt = new Set(oldShallow);
  for (const have of haves) {
    const { reachable } = store.collectReachable([have], stopAt);
    for (const oid of reachable) held.add(oid);
  }
  // Walk wants by parent depth; included commits are depth < limit and not held.
  const depthOf = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  const includedCommits = new Set<string>();
  const includedTags = new Set<string>();
  for (const want of wants) {
    const object = store.getObject(want);
    if (object === undefined) continue;
    if (object.type === 'tag') {
      includedTags.add(want);
      const target = parseTag(object.data).object;
      queue.push({ oid: target, depth: 0 });
    } else if (object.type === 'commit') {
      queue.push({ oid: want, depth: 0 });
    } else {
      // A blob/tree want: send it directly.
      includedCommits.add(want);
    }
  }
  const withinLimit = new Set<string>();
  // Index-based queue: Array.shift() is O(n) per pop.
  let head = 0;
  while (head < queue.length) {
    const { oid, depth: d } = queue[head++]!;
    const known = depthOf.get(oid);
    if (known !== undefined && known <= d) continue;
    depthOf.set(oid, d);
    if (d < limit) withinLimit.add(oid);
    const object = store.getObject(oid);
    if (object === undefined || object.type !== 'commit') continue;
    if (d < limit && !held.has(oid)) includedCommits.add(oid);
    if (d + 1 <= limit || limit === Number.POSITIVE_INFINITY) {
      const info = store.commitInfo(oid);
      if (info !== undefined) for (const parent of info.parents) queue.push({ oid: parent, depth: d + 1 });
    }
  }
  const shallow: string[] = [];
  if (Number.isFinite(limit)) {
    for (const oid of includedCommits) {
      if (depthOf.get(oid) !== limit - 1) continue;
      const info = store.commitInfo(oid);
      if (info !== undefined && info.parents.length > 0) {
        shallow.push(oid);
      }
    }
  }
  const unshallow: string[] = [];
  for (const old of oldShallow) {
    if (withinLimit.has(old) && !shallow.includes(old)) unshallow.push(old);
  }
  const toSend = new Set<string>();
  for (const oid of includedTags) toSend.add(oid);
  for (const oid of includedCommits) {
    toSend.add(oid);
    const info = store.commitInfo(oid);
    if (info === undefined) continue;
    const { reachable } = store.collectReachable([info.tree]);
    for (const entry of reachable) toSend.add(entry);
  }
  for (const oid of held) toSend.delete(oid);
  const objects: GitObject[] = [];
  const byType: Record<string, Array<{ oid: string; object: GitObject }>> = { tag: [], commit: [], tree: [], blob: [] };
  for (const oid of toSend) {
    const object = store.getObject(oid);
    if (object !== undefined) byType[object.type]!.push({ oid, object });
  }
  for (const type of ['tag', 'commit', 'tree', 'blob'] as const) {
    // Sort by oid string directly: hashing inside the comparator would
    // re-hash every object O(log n) times.
    const group = byType[type]!.sort((a, b) => (a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0));
    for (const entry of group) objects.push(entry.object);
  }
  const acks: string[] = [];
  for (const have of haves) {
    let common = false;
    for (const want of wants) {
      if (store.isAncestor(have, want)) {
        common = true;
        break;
      }
    }
    if (common) acks.push(have);
  }
  const wantedRefs: Array<{ oid: string; ref: string }> = [];
  for (const want of wants) {
    for (const ref of sortedRefs(repo)) {
      if (ref.oid === want) wantedRefs.push({ oid: want, ref: ref.name });
    }
  }
  return { objects, shallow, unshallow, acks, wantedRefs };
}

function sidebandPackets(pack: Buffer): Buffer[] {
  const packets: Buffer[] = [];
  const CHUNK = 32768;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const slice = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
    packets.push(encodePktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  if (packets.length === 0) packets.push(encodePktLine(Buffer.from([1])));
  return packets;
}

async function handleUploadPack(req: IncomingMessage, res: ServerResponse, owner: string, name: string, body: Buffer): Promise<void> {
  const repo = store.getRepo(owner, name);
  if (repo === undefined) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  let packets;
  try {
    packets = decodePktLines(body);
  } catch {
    sendJson(res, 400, { message: 'Malformed pkt-line stream' });
    return;
  }
  const lines: string[] = [];
  for (const packet of packets) {
    // Collect every data packet: a v0 fetch has two sections split by a flush
    // (wants, then haves), and both must be read.
    if (packet.kind === 'data') lines.push(packet.payload.toString('utf8'));
  }
  try {
    if (isV2(req)) {
      await handleUploadPackV2(req, res, repo, lines);
    } else {
      handleUploadPackV0(req, res, repo, lines);
    }
  } catch {
    sendJson(res, 400, { message: 'Malformed upload-pack request' });
  }
}

async function handleUploadPackV2(_req: IncomingMessage, res: ServerResponse, repo: Repo, lines: string[]): Promise<void> {
  const command = (lines[0] ?? '').trim();
  if (command === 'command=ls-refs') {
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    // Delim packets never reach this list (only data packets do), so every line
    // after the command is an argument; agent=/object-format= lines are skipped.
    for (const line of lines.slice(1)) {
      const text = line.trim();
      if (text === '' || text.startsWith('agent=') || text.startsWith('object-format=')) continue;
      if (text === 'peel') peel = true;
      else if (text === 'symrefs') symrefs = true;
      else if (text === 'unborn') unborn = true;
      else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
    }
    const out: Buffer[] = [];
    const mainTip = repo.refs.get(`refs/heads/${DEFAULT_BRANCH}`);
    const keep = (refName: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => refName.startsWith(prefix));
    if (keep('HEAD')) {
      if (mainTip !== undefined) {
        out.push(encodePktLine(`${mainTip} HEAD${symrefs ? ' symref-target:refs/heads/main' : ''}\n`));
      } else if (unborn) {
        out.push(encodePktLine('unborn HEAD symref-target:refs/heads/main\n'));
      }
    }
    for (const ref of sortedRefs(repo)) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (peel) {
        const peeled = peelOf(repo, ref.name, ref.oid);
        if (peeled !== null) line += ` peeled:${peeled}`;
      }
      out.push(encodePktLine(`${line}\n`));
    }
    out.push(Buffer.from('0000', 'ascii'));
    sendBytes(res, 200, Buffer.concat(out), 'application/x-git-upload-pack-result');
    return;
  }
  if (command === 'command=fetch') {
    const wants: string[] = [];
    const haves: string[] = [];
    const shallowIn: string[] = [];
    let depth: number | null = null;
    // Delim packets never reach this list (only data packets do), so every line
    // after the command is an argument; agent=/object-format= lines are skipped.
    for (const line of lines.slice(1)) {
      const text = line.trim();
      if (text === '' || text.startsWith('agent=') || text.startsWith('object-format=')) continue;
      if (text.startsWith('want ')) wants.push(text.slice(5).trim());
      else if (text.startsWith('have ')) haves.push(text.slice(5).trim());
      else if (text.startsWith('shallow ')) shallowIn.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) {
        const value = parseInt(text.slice(7).trim(), 10);
        if (Number.isFinite(value)) depth = value;
      }
      // thin-pack, no-progress, ofs-delta, filter, deepen-since, deepen-not, done: accepted.
    }
    const { plan, sideband } = fetchPack(repo, wants, haves, shallowIn, depth);
    const parts: Buffer[] = [];
    if (haves.length > 0) {
      parts.push(encodePktLine('acknowledgments\n'));
      if (plan.acks.length > 0) {
        for (const ack of plan.acks) parts.push(encodePktLine(`ACK ${ack}\n`));
        parts.push(encodePktLine('ready\n'));
      } else {
        parts.push(encodePktLine('NAK\n'));
      }
      parts.push(Buffer.from('0001', 'ascii'));
    }
    if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
      parts.push(encodePktLine('shallow-info\n'));
      for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
      for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
      parts.push(Buffer.from('0001', 'ascii'));
    }
    if (plan.wantedRefs.length > 0) {
      parts.push(encodePktLine('wanted-refs\n'));
      for (const entry of plan.wantedRefs) parts.push(encodePktLine(`${entry.oid} ${entry.ref}\n`));
      parts.push(Buffer.from('0001', 'ascii'));
    }
    parts.push(encodePktLine('packfile\n'));
    for (const packet of sideband) parts.push(packet);
    parts.push(Buffer.from('0000', 'ascii'));
    sendBytes(res, 200, Buffer.concat(parts), 'application/x-git-upload-pack-result');
    return;
  }
  sendJson(res, 400, { message: 'Unknown v2 command' });
}

function handleUploadPackV0(_req: IncomingMessage, res: ServerResponse, repo: Repo, lines: string[]): void {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowIn: string[] = [];
  let depth: number | null = null;
  // v0 lines are classified by keyword: `want`/`shallow`/`deepen` come in the first
  // section, `have`/`done` in the second, so no section tracking is needed.
  for (const line of lines) {
    const text = line.trim();
    if (text === '') continue;
    if (text.startsWith('want ')) {
      const rest = text.slice(5);
      const nul = rest.indexOf('\0');
      wants.push((nul >= 0 ? rest.slice(0, nul) : rest).trim());
    } else if (text.startsWith('have ')) {
      haves.push(text.slice(5).trim());
    } else if (text.startsWith('shallow ')) {
      shallowIn.push(text.slice(8).trim());
    } else if (text.startsWith('deepen ')) {
      const value = parseInt(text.slice(7).trim(), 10);
      if (Number.isFinite(value)) depth = value;
    }
  }
  const { plan, sideband } = fetchPack(repo, wants, haves, shallowIn, depth);
  const parts: Buffer[] = [];
  if (plan.acks.length > 0) {
    for (const ack of plan.acks) parts.push(encodePktLine(`ACK ${ack}\n`));
  } else {
    parts.push(encodePktLine('NAK\n'));
  }
  for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
  for (const packet of sideband) parts.push(packet);
  parts.push(Buffer.from('0000', 'ascii'));
  sendBytes(res, 200, Buffer.concat(parts), 'application/x-git-upload-pack-result');
}

async function handleReceivePack(req: IncomingMessage, res: ServerResponse, owner: string, name: string, body: Buffer): Promise<void> {
  const repo = store.getRepo(owner, name);
  if (repo === undefined) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  let commands: string[];
  let pack: Buffer;
  try {
    const split = splitCommands(body);
    commands = split.commands;
    pack = split.pack;
  } catch {
    sendJson(res, 400, { message: 'Malformed pkt-line stream' });
    return;
  }
  const report: Array<{ ref: string; ok: boolean; reason: string }> = [];
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (pack.length > 0) {
    try {
      if (!verifyPackChecksum(pack)) throw new Error('pack checksum mismatch');
      const { objects } = readPack(pack, (oid) => store.getObject(oid));
      store.addObjects(objects);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack failed';
    }
  }
  let sideband = true;
  for (const raw of commands) {
    const nul = raw.indexOf('\0');
    const head = nul >= 0 ? raw.slice(0, nul) : raw;
    if (nul >= 0) {
      const caps = raw.slice(nul + 1).trim().split(/\s+/);
      sideband = caps.includes('side-band-64k');
    }
    const parts = head.trim().split(/\s+/);
    if (parts.length < 3) continue;
    const [oldOid, newOid, ref] = parts as [string, string, string];
    const current = repo.refs.get(ref);
    if (!unpackOk) {
      report.push({ ref, ok: false, reason: 'unpack failed' });
      continue;
    }
    if (newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        report.push({ ref, ok: false, reason: 'remote ref does not exist' });
      } else if (current !== oldOid) {
        report.push({ ref, ok: false, reason: 'old value mismatch' });
      } else {
        repo.refs.delete(ref);
        report.push({ ref, ok: true, reason: '' });
      }
      continue;
    }
    if (oldOid === ZERO_OID) {
      // Create.
      if (current !== undefined) {
        report.push({ ref, ok: false, reason: 'already exists' });
        continue;
      }
    } else {
      if (current === undefined) {
        report.push({ ref, ok: false, reason: 'remote ref does not exist' });
        continue;
      }
      if (current !== oldOid) {
        report.push({ ref, ok: false, reason: 'old value mismatch' });
        continue;
      }
    }
    const object = store.getObject(newOid);
    if (object === undefined) {
      report.push({ ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (oldOid !== ZERO_OID && !store.isAncestor(oldOid, newOid)) {
      report.push({ ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(ref, newOid);
    report.push({ ref, ok: true, reason: '' });
  }
  const reportLines: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  for (const entry of report) {
    reportLines.push(entry.ok ? `ok ${entry.ref}\n` : `ng ${entry.ref} ${entry.reason}\n`);
  }
  // Real git (http-backend) terminates the report *inside* band 1 with a
  // flush packet, then ends the outer stream with a second flush. Without
  // the inner flush, git reads the report lines but reports
  // "the remote end hung up unexpectedly".
  const inner = Buffer.concat([encodePktLines(reportLines), Buffer.from('0000', 'ascii')]);
  let out: Buffer;
  if (sideband) {
    const parts: Buffer[] = [];
    for (let offset = 0; offset < inner.length; offset += 32768) {
      const slice = inner.subarray(offset, Math.min(inner.length, offset + 32768));
      parts.push(encodePktLine(Buffer.concat([Buffer.from([1]), slice])));
    }
    parts.push(Buffer.from('0000', 'ascii'));
    out = Buffer.concat(parts);
  } else {
    out = inner; // inner already ends with the terminating flush
  }
  sendBytes(res, 200, out, 'application/x-git-receive-pack-result');
}

// ---------------------------------------------------------------- server

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const method = req.method ?? 'GET';
        const target = parseTarget(req);
        const segments = target.segments;

        if (method === 'GET' && target.pathname === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }

        if (segments[0] === 'mcp') {
          await handleMcp(req, res, store, isAuthed);
          return;
        }

        if (segments[0] === 'api' && segments[1] === 'graphql') {
          if (method !== 'POST') {
            sendJson(res, 405, { message: 'Method Not Allowed' });
            return;
          }
          if (!isAuthed(req)) {
            sendJson(res, 401, { message: 'Requires authentication' });
            return;
          }
          await handleGraphQL(req, res, store);
          return;
        }

        if (segments[0] === 'api' && segments[1] === 'v3') {
          if (!isAuthed(req)) {
            sendJson(res, 401, { message: 'Requires authentication' });
            return;
          }
          await handleRest(req, res, segments.slice(2), target);
          return;
        }

        // Git smart HTTP: /<owner>/<repo>.git/<service>
        if (segments.length >= 2 && segments[1]!.endsWith('.git')) {
          const owner = segments[0]!;
          const name = segments[1]!.slice(0, -'.git'.length);
          const service = segments[2] ?? '';
          if (!isAuthed(req)) {
            sendJson(res, 401, { message: 'Requires authentication' });
            return;
          }
          if (method === 'GET' && service === 'info' && segments[3] === 'refs') {
            await handleInfoRefs(req, res, owner, name);
            return;
          }
          if (method === 'POST' && service === 'git-upload-pack') {
            const body = await readBody(req);
            await handleUploadPack(req, res, owner, name, body);
            return;
          }
          if (method === 'POST' && service === 'git-receive-pack') {
            const body = await readBody(req);
            await handleReceivePack(req, res, owner, name, body);
            return;
          }
          notFound(res);
          return;
        }

        notFound(res);
      } catch (err) {
        try {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
        } catch {
          // The socket is already gone.
        }
      }
    })();
  });
}

// Keep the store importable for the other endpoints (single instance per process).
export { store };
