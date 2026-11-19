// graphql.ts — a small GraphQL v4 executor for exactly the documents gh and
// github-mcp-server send: viewer/repository/node/__type reads, issue and pull
// request connections, and the create/close/merge/comment mutations.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  FIXED_ISO,
  ForgeStore,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  globalId,
  parseGlobalId,
  type Issue,
  type Pull,
  type Repo,
} from './store.ts';

// ---------------------------------------------------------------- lexer

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'int'; value: string }
  | { kind: 'float'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'punct'; value: string }
  | { kind: 'eof' };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const isNameStart = (ch: string): boolean => /[A-Za-z_]/.test(ch);
  const isNameChar = (ch: string): boolean => /[A-Za-z0-9_]/.test(ch);
  while (i < source.length) {
    const ch = source[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}():!=$@[]='.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      if (source.slice(i, i + 3) === '"""') {
        const end = source.indexOf('"""', i + 3);
        const raw = end < 0 ? source.slice(i + 3) : source.slice(i + 3, end);
        tokens.push({ kind: 'string', value: raw });
        i = end < 0 ? source.length : end + 3;
        continue;
      }
      let j = i + 1;
      let value = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\' && j + 1 < source.length) {
          const esc = source[j + 1]!;
          value += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc === 'u' ? JSON.parse(`"\\u${source.slice(j + 2, j + 6)}"`) as string : esc;
          j += esc === 'u' ? 6 : 2;
          continue;
        }
        value += source[j];
        j += 1;
      }
      tokens.push({ kind: 'string', value });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      let float = false;
      if (source[j] === '-') j += 1;
      while (j < source.length && /[0-9]/.test(source[j]!)) j += 1;
      if (source[j] === '.' && /[0-9]/.test(source[j + 1] ?? '')) {
        float = true;
        j += 1;
        while (j < source.length && /[0-9]/.test(source[j]!)) j += 1;
      }
      if (source[j] === 'e' || source[j] === 'E') {
        float = true;
        j += 1;
        if (source[j] === '+' || source[j] === '-') j += 1;
        while (j < source.length && /[0-9]/.test(source[j]!)) j += 1;
      }
      tokens.push(float ? { kind: 'float', value: source.slice(i, j) } : { kind: 'int', value: source.slice(i, j) });
      i = j;
      continue;
    }
    if (isNameStart(ch)) {
      let j = i;
      while (j < source.length && isNameChar(source[j]!)) j += 1;
      tokens.push({ kind: 'name', value: source.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)}`);
  }
  tokens.push({ kind: 'eof' });
  return tokens;
}

// ---------------------------------------------------------------- parser

interface Selection {
  alias?: string;
  name: string;
  args: Record<string, unknown>;
  selections: Selection[];
  fragment?: string;
  inlineType?: string;
}

interface Operation {
  kind: 'query' | 'mutation';
  name?: string;
  selections: Selection[];
}

interface Document {
  operations: Operation[];
  fragments: Map<string, { type: string; selections: Selection[] }>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos]!;
  }

  private next(): Token {
    return this.tokens[this.pos++]!;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}`);
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error('expected a name');
    return token.value;
  }

  parseDocument(): Document {
    const operations: Operation[] = [];
    const fragments = new Map<string, { type: string; selections: Selection[] }>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName(); // 'on'
        const type = this.expectName();
        const selections = this.parseSelectionSet();
        fragments.set(name, { type, selections });
        continue;
      }
      operations.push(this.parseOperation());
    }
    return { operations, fragments };
  }

  private parseOperation(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    const token = this.peek();
    if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      kind = this.next().value as 'query' | 'mutation';
    }
    let name: string | undefined;
    if (this.peek().kind === 'name') name = this.next().value;
    if (this.peek().kind === 'punct' && this.peek().value === '(') this.skipParens();
    return { kind, name, selections: this.parseSelectionSet() };
  }

  private skipParens(): void {
    this.expectPunct('(');
    let depth = 1;
    while (depth > 0) {
      const token = this.next();
      if (token.kind === 'eof') throw new Error('unbalanced parens');
      if (token.kind === 'punct' && token.value === '(') depth += 1;
      if (token.kind === 'punct' && token.value === ')') depth -= 1;
    }
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      if (this.peek().kind === 'eof') throw new Error('unbalanced selection set');
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.peek().kind === 'punct' && this.peek().value === '...') {
      this.next();
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'on') {
        this.next();
        const type = this.expectName();
        return { name: '', args: {}, selections: this.parseSelectionSet(), inlineType: type };
      }
      const name = this.expectName();
      return { name: '', args: {}, selections: [], fragment: name };
    }
    let alias: string | undefined;
    let name = this.expectName();
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    let args: Record<string, unknown> = {};
    if (this.peek().kind === 'punct' && this.peek().value === '(') args = this.parseArguments();
    // Directives (@include/@skip): parsed and ignored.
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.skipParens();
    }
    let selections: Selection[] = [];
    if (this.peek().kind === 'punct' && this.peek().value === '{') selections = this.parseSelectionSet();
    return { alias, name, args, selections };
  }

  private parseArguments(): Record<string, unknown> {
    this.expectPunct('(');
    const args: Record<string, unknown> = {};
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args[name] = this.parseValue();
    }
    this.expectPunct(')');
    return args;
  }

  private parseValue(): unknown {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { __var: this.expectName() };
    if (token.kind === 'punct' && token.value === '[') {
      const items: unknown[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) items.push(this.parseValue());
      this.expectPunct(']');
      return items;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const object: Record<string, unknown> = {};
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        object[name] = this.parseValue();
      }
      this.expectPunct('}');
      return object;
    }
    if (token.kind === 'string') return token.value;
    if (token.kind === 'int') return parseInt(token.value, 10);
    if (token.kind === 'float') return parseFloat(token.value);
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return token.value; // enum value
    }
    throw new Error('unexpected value');
  }
}

function resolveVariables(value: unknown, variables: Record<string, unknown>): unknown {
  if (typeof value === 'object' && value !== null) {
    const record = value as Record<string, unknown>;
    if ('__var' in record && typeof record.__var === 'string') return variables[record.__var] ?? null;
    if (Array.isArray(value)) return value.map((entry) => resolveVariables(entry, variables));
    const out: Record<string, unknown> = {};
    for (const [key, entry] of Object.entries(record)) out[key] = resolveVariables(entry, variables);
    return out;
  }
  return value;
}

// ---------------------------------------------------------------- executor

type GqlValue = Record<string, unknown> & { __t?: string };

interface Ctx {
  store: ForgeStore;
  errors: Array<{ message: string }>;
  fragments: Map<string, { type: string; selections: Selection[] }>;
  variables: Record<string, unknown>;
}

const ISSUE_FIELDS = ['id', 'databaseId', 'number', 'title', 'body', 'state', 'url', 'author', 'labels', 'comments', 'createdAt', 'updatedAt', 'closedAt', 'repository'];
const PULL_FIELDS = ['id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'url', 'author', 'createdAt', 'updatedAt', 'closedAt', 'mergedAt', 'mergeCommit'];
const REPO_FIELDS = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'isPrivate', 'description', 'url', 'issue', 'issues', 'pullRequest', 'pullRequests', 'labels', 'createdAt', 'updatedAt'];
const USER_FIELDS = ['login', 'id', 'name', 'avatarUrl', 'url', 'email'];
const COMMENT_FIELDS = ['id', 'body', 'author', 'createdAt', 'updatedAt'];
const REF_FIELDS = ['name', 'prefix', 'target'];
const CONNECTION_FIELDS = ['nodes', 'edges', 'totalCount', 'pageInfo'];
const PAGE_INFO_FIELDS = ['hasNextPage', 'hasPreviousPage', 'endCursor', 'startCursor'];
const TYPE_FIELDS = ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'inputFields', 'interfaces', 'ofType'];

function fieldKnown(type: string, field: string): boolean {
  switch (type) {
    case 'Query':
      return ['viewer', 'repository', 'node', '__type', '__schema'].includes(field);
    case 'Mutation':
      return ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'].includes(field);
    case 'Repository':
      return REPO_FIELDS.includes(field);
    case 'Issue':
      return ISSUE_FIELDS.includes(field);
    case 'PullRequest':
      return PULL_FIELDS.includes(field);
    case 'User':
      return USER_FIELDS.includes(field);
    case 'IssueComment':
      return COMMENT_FIELDS.includes(field);
    case 'Ref':
      return REF_FIELDS.includes(field);
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'IssueCommentConnection':
      return CONNECTION_FIELDS.includes(field);
    case 'PageInfo':
      return PAGE_INFO_FIELDS.includes(field);
    case '__Type':
      return TYPE_FIELDS.includes(field);
    case 'IssueEdge':
    case 'PullRequestEdge':
    case 'IssueCommentEdge':
    case 'PullRequestChangedFileEdge':
      return ['node', 'cursor'].includes(field);
    case 'PullRequestChangedFile':
      return ['path', 'additions', 'deletions', 'changeType', 'status'].includes(field);
    case 'Label':
      return ['id', 'name', 'color', 'description'].includes(field);
    case 'Commit':
      return ['oid', 'message', 'author', 'committer'].includes(field);
    case 'AddIssueCommentPayload':
    case 'CreateIssuePayload':
    case 'CloseIssuePayload':
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload':
      return ['issue', 'pullRequest', 'commentEdge', 'clientMutationId'].includes(field);
    default:
      return false;
  }
}

function typeOf(value: GqlValue): string {
  return value.__t ?? 'Unknown';
}

function stripInternal(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripInternal);
  if (typeof value === 'object' && value !== null) {
    const out: Record<string, unknown> = {};
    for (const [key, entry] of Object.entries(value as Record<string, unknown>)) {
      if (key.startsWith('__')) continue;
      out[key] = stripInternal(entry);
    }
    return out;
  }
  return value;
}

function repoValue(store: ForgeStore, repo: Repo): GqlValue {
  return {
    __t: 'Repository',
    __repo: repo,
    id: globalId('Repository', `${repo.owner}/${repo.name}`),
    databaseId: repo.dbId,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    isPrivate: repo.private,
    description: repo.description,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
  };
}

function userValue(): GqlValue {
  return { __t: 'User', login: VIEWER_LOGIN, id: globalId('User', VIEWER_LOGIN), name: VIEWER_NAME, email: '', avatarUrl: '', url: '' };
}

function issueValue(store: ForgeStore, repo: Repo, issue: Issue): GqlValue {
  return {
    __t: 'Issue',
    __repo: repo,
    __issue: issue,
    id: globalId('Issue', `${repo.owner}/${repo.name}#${issue.number}`),
    databaseId: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
    closedAt: issue.state === 'closed' ? FIXED_ISO : null,
  };
}

function pullValue(store: ForgeStore, repo: Repo, pull: Pull): GqlValue {
  return {
    __t: 'PullRequest',
    __repo: repo,
    __pull: pull,
    id: globalId('PullRequest', `${repo.owner}/${repo.name}#${pull.number}`),
    databaseId: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED',
    merged: pull.merged,
    headRefName: pull.headRef,
    baseRefName: pull.baseRef,
    headRefOid: pull.headSha,
    baseRefOid: pull.baseSha,
    mergedAt: pull.mergedAt,
    createdAt: FIXED_ISO,
    updatedAt: FIXED_ISO,
    closedAt: pull.state === 'closed' ? FIXED_ISO : null,
  };
}

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberAfter(after: unknown): number | null {
  if (typeof after !== 'string' || after === '') return null;
  try {
    const text = Buffer.from(after, 'base64').toString('utf8');
    if (!text.startsWith('cursor:')) return null;
    const value = parseInt(text.slice('cursor:'.length), 10);
    return Number.isFinite(value) ? value : null;
  } catch {
    return null;
  }
}

function issuesConnection(ctx: Ctx, repo: Repo, args: Record<string, unknown>): GqlValue {
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  const orderBy = (args.orderBy as Record<string, unknown> | undefined) ?? undefined;
  const after = numberAfter(args.after);
  const first = typeof args.first === 'number' ? args.first : 100;
  let issues = [...repo.issues.values()];
  if (states !== null) {
    const wanted = new Set(states.map((state) => String(state).toUpperCase()));
    issues = issues.filter((issue) => wanted.has(issue.state === 'open' ? 'OPEN' : 'CLOSED'));
  }
  const direction = String(orderBy?.direction ?? 'ASC').toUpperCase();
  issues.sort((a, b) => (direction === 'DESC' ? b.seq - a.seq : a.seq - b.seq));
  if (after !== null) issues = issues.filter((issue) => (direction === 'DESC' ? issue.number < after : issue.number > after));
  const total = issues.length;
  const page = issues.slice(0, Math.max(0, first));
  const endCursor = page.length > 0 ? cursorFor(page[page.length - 1]!.number) : null;
  return {
    __t: 'IssueConnection',
    __nodes: page.map((issue) => issueValue(ctx.store, repo, issue)),
    __total: total,
    __pageInfo: {
      __t: 'PageInfo',
      hasNextPage: total > page.length,
      hasPreviousPage: false,
      endCursor,
      startCursor: page.length > 0 ? cursorFor(page[0]!.number) : null,
    },
  };
}

function pullsConnection(ctx: Ctx, repo: Repo, args: Record<string, unknown>): GqlValue {
  const after = numberAfter(args.after);
  const first = typeof args.first === 'number' ? args.first : 100;
  let pulls = [...repo.pulls.values()].sort((a, b) => a.seq - b.seq);
  if (after !== null) pulls = pulls.filter((pull) => pull.number > after);
  const total = pulls.length;
  const page = pulls.slice(0, Math.max(0, first));
  const endCursor = page.length > 0 ? cursorFor(page[page.length - 1]!.number) : null;
  return {
    __t: 'PullRequestConnection',
    __nodes: page.map((pull) => pullValue(ctx.store, repo, pull)),
    __total: total,
    __pageInfo: {
      __t: 'PageInfo',
      hasNextPage: total > page.length,
      hasPreviousPage: false,
      endCursor,
      startCursor: page.length > 0 ? cursorFor(page[0]!.number) : null,
    },
  };
}

function resolveField(ctx: Ctx, parent: GqlValue, field: Selection): unknown {
  const type = typeOf(parent);
  const name = field.name;
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const store = ctx.store;

  if (type === 'Query') {
    if (name === 'viewer') return userValue();
    if (name === 'repository') {
      const repo = store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
      return repo === undefined ? null : repoValue(store, repo);
    }
    if (name === 'node') {
      const parsed = parseGlobalId(String(args.id ?? ''));
      if (parsed === null) return null;
      if (parsed.kind === 'Repository') {
        const [owner, repoName] = parsed.key.split('/');
        const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
        return repo === undefined ? null : repoValue(store, repo);
      }
      if (parsed.kind === 'Issue') {
        const [ownerRepo, numberText] = parsed.key.split('#');
        const [owner, repoName] = (ownerRepo ?? '').split('/');
        const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
        const issue = repo?.issues.get(parseInt(numberText ?? '', 10));
        return issue === undefined || repo === undefined ? null : issueValue(store, repo, issue);
      }
      if (parsed.kind === 'PullRequest') {
        const [ownerRepo, numberText] = parsed.key.split('#');
        const [owner, repoName] = (ownerRepo ?? '').split('/');
        const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
        const pull = repo?.pulls.get(parseInt(numberText ?? '', 10));
        return pull === undefined || repo === undefined ? null : pullValue(store, repo, pull);
      }
      return null;
    }
    if (name === '__type') {
      return introspectionType(String(args.name ?? ''));
    }
    if (name === '__schema') {
      return { __t: '__Schema', queryType: { __t: '__Type', name: 'Query' }, mutationType: { __t: '__Type', name: 'Mutation' } };
    }
  }

  if (type === 'Mutation') {
    if (name === 'createIssue') {
      const input = (args.input as Record<string, unknown>) ?? {};
      const parsed = parseGlobalId(String(input.repositoryId ?? ''));
      if (parsed === null || parsed.kind !== 'Repository') {
        ctx.errors.push({ message: 'createIssue: unknown repositoryId' });
        return null;
      }
      const [owner, repoName] = parsed.key.split('/');
      const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
      if (repo === undefined) {
        ctx.errors.push({ message: 'createIssue: repository not found' });
        return null;
      }
      const created = store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''), []);
      if (typeof created === 'object' && created !== null && 'status' in created) {
        ctx.errors.push({ message: `createIssue: ${(created as { message: string }).message}` });
        return null;
      }
      return { __t: 'CreateIssuePayload', issue: issueValue(store, repo, created as Issue) };
    }
    if (name === 'closeIssue') {
      const input = (args.input as Record<string, unknown>) ?? {};
      const parsed = parseGlobalId(String(input.issueId ?? ''));
      if (parsed === null || parsed.kind !== 'Issue') {
        ctx.errors.push({ message: 'closeIssue: unknown issueId' });
        return null;
      }
      const [ownerRepo, numberText] = parsed.key.split('#');
      const [owner, repoName] = (ownerRepo ?? '').split('/');
      const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
      const issue = repo?.issues.get(parseInt(numberText ?? '', 10));
      if (issue === undefined || repo === undefined) {
        ctx.errors.push({ message: 'closeIssue: issue not found' });
        return null;
      }
      issue.state = 'closed';
      store.touchIssue(issue);
      return { __t: 'CloseIssuePayload', issue: issueValue(store, repo, issue) };
    }
    if (name === 'createPullRequest') {
      const input = (args.input as Record<string, unknown>) ?? {};
      const parsed = parseGlobalId(String(input.repositoryId ?? ''));
      if (parsed === null || parsed.kind !== 'Repository') {
        ctx.errors.push({ message: 'createPullRequest: unknown repositoryId' });
        return null;
      }
      const [owner, repoName] = parsed.key.split('/');
      const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
      if (repo === undefined) {
        ctx.errors.push({ message: 'createPullRequest: repository not found' });
        return null;
      }
      const created = store.createPull(repo, String(input.title ?? ''), String(input.body ?? ''), String(input.headRefName ?? ''), String(input.baseRefName ?? ''));
      if (typeof created === 'object' && created !== null && 'status' in created) {
        ctx.errors.push({ message: `createPullRequest: ${(created as { message: string }).message}` });
        return null;
      }
      return { __t: 'CreatePullRequestPayload', pullRequest: pullValue(store, repo, created as Pull) };
    }
    if (name === 'mergePullRequest') {
      const input = (args.input as Record<string, unknown>) ?? {};
      const parsed = parseGlobalId(String(input.pullRequestId ?? ''));
      if (parsed === null || parsed.kind !== 'PullRequest') {
        ctx.errors.push({ message: 'mergePullRequest: unknown pullRequestId' });
        return null;
      }
      const [ownerRepo, numberText] = parsed.key.split('#');
      const [owner, repoName] = (ownerRepo ?? '').split('/');
      const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
      const pull = repo?.pulls.get(parseInt(numberText ?? '', 10));
      if (pull === undefined || repo === undefined) {
        ctx.errors.push({ message: 'mergePullRequest: pull request not found' });
        return null;
      }
      const merged = store.mergePull(repo, pull);
      if (typeof merged === 'object' && merged !== null && 'status' in merged) {
        ctx.errors.push({ message: `mergePullRequest: ${(merged as { message: string }).message}` });
        return null;
      }
      return { __t: 'MergePullRequestPayload', pullRequest: pullValue(store, repo, pull) };
    }
    if (name === 'addComment') {
      const input = (args.input as Record<string, unknown>) ?? {};
      const parsed = parseGlobalId(String(input.subjectId ?? ''));
      if (parsed === null || parsed.kind !== 'Issue') {
        ctx.errors.push({ message: 'addComment: unknown subjectId' });
        return null;
      }
      const [ownerRepo, numberText] = parsed.key.split('#');
      const [owner, repoName] = (ownerRepo ?? '').split('/');
      const repo = owner !== undefined && repoName !== undefined ? store.getRepo(owner, repoName) : undefined;
      const issue = repo?.issues.get(parseInt(numberText ?? '', 10));
      if (issue === undefined || repo === undefined) {
        ctx.errors.push({ message: 'addComment: issue not found' });
        return null;
      }
      const comment = { id: store.nextId++, body: String(input.body ?? '') };
      issue.comments.push(comment);
      store.touchIssue(issue);
      const node: GqlValue = {
        __t: 'IssueComment',
        id: globalId('IssueComment', `${repo.owner}/${repo.name}#${issue.number}#${comment.id}`),
        body: comment.body,
        __comment: comment,
        __repo: repo,
        __issue: issue,
      };
      return { __t: 'AddIssueCommentPayload', commentEdge: { __t: 'IssueCommentEdge', node, cursor: cursorFor(comment.id) } };
    }
  }

  if (type === 'Repository') {
    const repo = parent.__repo as Repo;
    if (name === 'owner') return userValue();
    if (name === 'defaultBranchRef') return { __t: 'Ref', name: repo.defaultBranch, prefix: 'refs/heads/' };
    if (name === 'issue') {
      const issue = repo.issues.get(Number(args.number));
      return issue === undefined ? null : issueValue(store, repo, issue);
    }
    if (name === 'issues') return issuesConnection(ctx, repo, args);
    if (name === 'pullRequest') {
      const pull = repo.pulls.get(Number(args.number));
      return pull === undefined ? null : pullValue(store, repo, pull);
    }
    if (name === 'pullRequests') return pullsConnection(ctx, repo, args);
    if (name === 'labels') {
      return {
        __t: 'LabelConnection',
        __nodes: [...repo.labels.values()].map((label) => ({ __t: 'Label', id: label.id, name: label.name, color: label.color, description: '' })),
        __total: repo.labels.size,
        __pageInfo: { __t: 'PageInfo', hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null },
      };
    }
    const value = (parent as Record<string, unknown>)[name];
    return value === undefined ? null : value;
  }

  if (type === 'Issue') {
    const repo = parent.__repo as Repo;
    const issue = parent.__issue as Issue;
    if (name === 'author') return userValue();
    if (name === 'repository') return repoValue(store, repo);
    if (name === 'labels') {
      return {
        __t: 'LabelConnection',
        __nodes: issue.labels.map((labelName) => ({ __t: 'Label', id: repo.labels.get(labelName)?.id ?? 0, name: labelName, color: repo.labels.get(labelName)?.color ?? 'ededed', description: '' })),
        __total: issue.labels.length,
        __pageInfo: { __t: 'PageInfo', hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null },
      };
    }
    if (name === 'comments') {
      const first = typeof args.first === 'number' ? args.first : 100;
      const nodes = issue.comments.slice(0, Math.max(0, first)).map((comment) => ({
        __t: 'IssueComment',
        id: globalId('IssueComment', `${repo.owner}/${repo.name}#${issue.number}#${comment.id}`),
        body: comment.body,
        __comment: comment,
        __repo: repo,
        __issue: issue,
      }));
      return {
        __t: 'IssueCommentConnection',
        __nodes: nodes,
        __total: issue.comments.length,
        __pageInfo: { __t: 'PageInfo', hasNextPage: issue.comments.length > nodes.length, hasPreviousPage: false, endCursor: null, startCursor: null },
      };
    }
    const value = (parent as Record<string, unknown>)[name];
    return value === undefined ? null : value;
  }

  if (type === 'PullRequest') {
    const repo = parent.__repo as Repo;
    const pull = parent.__pull as Pull;
    if (name === 'author') return userValue();
    if (name === 'mergeable') return store.pullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
    if (name === 'files') {
      const first = typeof args.first === 'number' ? args.first : 100;
      const files = store.pullFiles(repo, pull).slice(0, Math.max(0, first));
      return {
        __t: 'PullRequestChangedFileConnection',
        __nodes: files.map((file) => ({ __t: 'PullRequestChangedFile', path: file.filename, additions: file.additions, deletions: file.deletions, changeType: file.status === 'added' ? 'ADDED' : file.status === 'removed' ? 'DELETED' : 'MODIFIED', status: file.status })),
        __total: files.length,
        __pageInfo: { __t: 'PageInfo', hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null },
      };
    }
    if (name === 'mergeCommit') {
      return pull.mergeSha === null ? null : { __t: 'Commit', oid: pull.mergeSha, message: '' };
    }
    const value = (parent as Record<string, unknown>)[name];
    return value === undefined ? null : value;
  }

  if (type === 'IssueComment') {
    if (name === 'author') return userValue();
    const value = (parent as Record<string, unknown>)[name];
    return value === undefined ? null : value;
  }

  if (type.endsWith('Connection')) {
    if (name === 'nodes') return (parent as Record<string, unknown>).__nodes ?? [];
    if (name === 'edges') {
      const nodes = ((parent as Record<string, unknown>).__nodes as GqlValue[] | undefined) ?? [];
      const edgeType = type.replace('Connection', 'Edge');
      return nodes.map((node, index) => ({ __t: edgeType, node, cursor: cursorFor(index + 1) }));
    }
    if (name === 'totalCount') return (parent as Record<string, unknown>).__total ?? 0;
    if (name === 'pageInfo') return (parent as Record<string, unknown>).__pageInfo ?? null;
  }

  if (type.endsWith('Edge')) {
    if (name === 'node') return (parent as Record<string, unknown>).node ?? null;
    if (name === 'cursor') return (parent as Record<string, unknown>).cursor ?? null;
  }

  if (type === '__Type') {
    const value = (parent as Record<string, unknown>)[name];
    return value === undefined ? null : value;
  }

  const value = (parent as Record<string, unknown>)[name];
  return value === undefined ? null : value;
}

function introspectionType(name: string): GqlValue {
  const fieldsFor: Record<string, string[]> = {
    Issue: ISSUE_FIELDS,
    PullRequest: PULL_FIELDS,
    Repository: REPO_FIELDS,
    User: USER_FIELDS,
    IssueComment: COMMENT_FIELDS,
  };
  const enums: Record<string, string[]> = {
    SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE'],
    IssueState: ['OPEN', 'CLOSED'],
    PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
    MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
    RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
    OrderDirection: ['ASC', 'DESC'],
    IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
    PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  };
  if (enums[name] !== undefined) {
    return {
      __t: '__Type',
      name,
      kind: 'ENUM',
      description: '',
      fields: null,
      enumValues: enums[name]!.map((entry) => ({ name: entry, description: '' })),
    };
  }
  const fields = fieldsFor[name];
  if (fields === undefined) return { __t: '__Type', name, kind: 'OBJECT', description: '', fields: [], enumValues: null };
  return {
    __t: '__Type',
    name,
    kind: 'OBJECT',
    description: '',
    fields: fields.map((entry) => ({ name: entry, description: '' })),
    enumValues: null,
  };
}

/** Expand fragments/inline fragments in a selection list. */
function expandSelections(ctx: Ctx, selections: Selection[], onType: string): Selection[] {
  const out: Selection[] = [];
  for (const selection of selections) {
    if (selection.fragment !== undefined) {
      const fragment = ctx.fragments.get(selection.fragment);
      if (fragment !== undefined) out.push(...expandSelections(ctx, fragment.selections, onType));
      continue;
    }
    if (selection.inlineType !== undefined) {
      if (selection.inlineType === onType) out.push(...expandSelections(ctx, selection.selections, onType));
      continue;
    }
    out.push(selection);
  }
  return out;
}

function executeSelections(ctx: Ctx, parent: GqlValue, selections: Selection[]): Record<string, unknown> {
  const type = typeOf(parent);
  const expanded = expandSelections(ctx, selections, type);
  const out: Record<string, unknown> = {};
  for (const selection of expanded) {
    const responseKey = selection.alias ?? selection.name;
    if (!fieldKnown(type, selection.name)) {
      ctx.errors.push({ message: `Cannot query field "${selection.name}" on type "${type}".` });
      out[responseKey] = null;
      continue;
    }
    const value = resolveField(ctx, parent, selection);
    out[responseKey] = selectValue(ctx, value, selection.selections);
  }
  return out;
}

function selectValue(ctx: Ctx, value: unknown, selections: Selection[]): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => selectValue(ctx, entry, selections));
  if (typeof value === 'object') {
    const record = value as GqlValue;
    if (record.__t !== undefined && selections.length > 0) {
      return executeSelections(ctx, record, selections);
    }
    return stripInternal(record);
  }
  return value;
}

function executeOperation(ctx: Ctx, operation: Operation): { data: Record<string, unknown> | null; errors: Array<{ message: string }> } {
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const root: GqlValue = { __t: rootType };
  const data = executeSelections(ctx, root, operation.selections);
  return { data, errors: ctx.errors };
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
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

export async function handleGraphQL(req: IncomingMessage, res: ServerResponse, store: ForgeStore): Promise<void> {
  const raw = await readBody(req);
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw.toString('utf8'));
  } catch {
    sendJson(res, 400, { errors: [{ message: 'Invalid JSON body' }] });
    return;
  }
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (typeof entry !== 'object' || entry === null) return { data: null, errors: [{ message: 'Invalid GraphQL request' }] };
    const record = entry as Record<string, unknown>;
    const query = typeof record.query === 'string' ? record.query : '';
    const variables = (record.variables as Record<string, unknown> | undefined) ?? {};
    let document: Document;
    try {
      document = new Parser(lex(query)).parseDocument();
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Syntax error' }] };
    }
    const operation = document.operations[0];
    if (operation === undefined) return { data: null, errors: [{ message: 'No operation in document' }] };
    const ctx: Ctx = { store, errors: [], fragments: document.fragments, variables };
    const result = executeOperation(ctx, operation);
    const out: Record<string, unknown> = { data: result.data };
    if (result.errors.length > 0) out.errors = result.errors;
    return out;
  };
  if (Array.isArray(parsed)) {
    sendJson(res, 200, parsed.map(runOne));
    return;
  }
  sendJson(res, 200, runOne(parsed));
}
