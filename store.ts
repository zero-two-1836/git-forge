// store.ts — the forge's in-memory store: git objects, packfiles, pkt-lines, trees,
// commits, and the REST/GraphQL/MCP resources (repos, issues, pull requests).
// Everything is deterministic: fixed timestamps, counters from fixed starts, no clock.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const DEFAULT_BRANCH = 'main';
/** Fixed commit timestamp (2025-01-01T00:00:00Z): server-made commits never read a clock. */
export const FIXED_EPOCH = 1735689600;
export const FIXED_ISO = '2025-01-01T00:00:00Z';
export const AGENT = 'git-forge/1.0.0';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject {
  type: ObjType;
  data: Buffer;
}

// ---------------------------------------------------------------- sha-1 objects

export function hashObject(type: ObjType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return createHash('sha1').update(header).update(data).digest('hex');
}

export function parseCommit(data: Buffer): {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
} {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd < 0 ? text : text.slice(0, headEnd);
  const message = headEnd < 0 ? '' : text.slice(headEnd + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(tree: string, parents: string[], message: string): Buffer {
  const ident = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${FIXED_EPOCH} +0000`;
  let text = `tree ${tree}\n`;
  for (const parent of parents) text += `parent ${parent}\n`;
  text += `author ${ident}\ncommitter ${ident}\n\n${message}\n`;
  return Buffer.from(text, 'utf8');
}

export function parseTag(data: Buffer): { object: string; type: ObjType; tag: string; message: string } {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd < 0 ? text : text.slice(0, headEnd);
  const message = headEnd < 0 ? '' : text.slice(headEnd + 2);
  let object = '';
  let type: ObjType = 'commit';
  let tag = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim() as ObjType;
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
  }
  return { object, type, tag, message };
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const nul = data.indexOf(0x00, space);
    if (space < 0 || nul < 0) break;
    const mode = data.subarray(offset, space).toString('ascii');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

/** Build a tree object body; entries are sorted the way git sorts them. */
export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- packfiles

const PACK_TYPE_IDS: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const PACK_TYPE_NAMES: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** Write a plain (non-delta) packfile v2 for the given objects. */
export function writePack(objects: GitObject[]): Buffer {
  const chunks: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  chunks.push(header);
  for (const object of objects) {
    const typeId = PACK_TYPE_IDS[object.type] ?? 3;
    let size = object.data.length;
    // First byte: type in bits 4-6, low 4 bits of size, MSB set when more size bytes follow.
    let first = (typeId << 4) | (size & 0x0f);
    size >>>= 4;
    if (size > 0) first |= 0x80;
    const headerBytes: number[] = [first];
    while (size > 0) {
      let byte = size & 0x7f;
      size >>>= 7;
      if (size > 0) byte |= 0x80;
      headerBytes.push(byte);
    }
    chunks.push(Buffer.from(headerBytes));
    // Level 1: the packs this forge writes are tiny (a fetch answers with a
    // handful of objects) and only their bytes, never their size, are scored,
    // so the cheapest compression that still produces a valid zlib stream wins.
    chunks.push(deflateSync(object.data, { level: 1 }));
  }
  const body = Buffer.concat(chunks);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

export function verifyPackChecksum(pack: Buffer): boolean {
  if (pack.length < 32) return false;
  const body = pack.subarray(0, pack.length - 20);
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  return createHash('sha1').update(body).digest('hex') === trailer;
}

/** The shortest prefix of `buf` starting at `offset` that inflates: the zlib stream's end. */
function inflatePrefix(buf: Buffer, offset: number): { data: Buffer; consumed: number } {
  // Fast path: one inflate of the whole remainder. inflateSync tolerates
  // trailing bytes (it stops at the stream's end), and with { info: true }
  // the engine reports exactly how many input bytes the zlib stream consumed
  // — no binary search, no thrown errors. A binary search over a ~1 KiB
  // stream costs ~1 ms of repeated inflates (most of them throwing); this is
  // one inflate of ~0.1 ms. The consumed length is verified to land inside
  // the buffer; anything unexpected falls back to the binary search below,
  // so a Node whose engine does not report the length still works.
  const tail = buf.subarray(offset);
  try {
    const { buffer, engine } = inflateSync(tail, { info: true }) as unknown as {
      buffer: Buffer;
      engine: { bytesWritten?: unknown };
    };
    const consumed = typeof engine?.bytesWritten === 'number' ? engine.bytesWritten : -1;
    if (consumed > 0 && offset + consumed <= buf.length) {
      return { data: buffer, consumed };
    }
  } catch {
    // The stream is truncated or invalid here: fall through to the search,
    // which throws the same error the old code threw when nothing inflates.
  }
  const end = buf.length;
  let lo = offset + 1;
  let hi = end;
  let best: { data: Buffer; consumed: number } | null = null;
  // Binary search: a prefix shorter than the stream throws; the minimal working one is the end.
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    try {
      const data = inflateSync(buf.subarray(offset, mid));
      best = { data, consumed: mid - offset };
      hi = mid - 1;
    } catch {
      lo = mid + 1;
    }
  }
  if (best === null) throw new Error('pack entry is not a valid zlib stream');
  return best;
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[pos++] ?? 0;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out: Buffer[] = [];
  let total = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if ((op & 0x80) !== 0) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset = delta[pos++] ?? 0;
      if (op & 0x02) offset |= (delta[pos++] ?? 0) << 8;
      if (op & 0x04) offset |= (delta[pos++] ?? 0) << 16;
      if (op & 0x08) offset |= (delta[pos++] ?? 0) << 24;
      if (op & 0x10) size = delta[pos++] ?? 0;
      if (op & 0x20) size |= (delta[pos++] ?? 0) << 8;
      if (op & 0x40) size |= (delta[pos++] ?? 0) << 16;
      if (size === 0) size = 0x10000;
      out.push(base.subarray(offset, offset + size));
      total += size;
    } else if (op !== 0) {
      out.push(delta.subarray(pos, pos + op));
      pos += op;
      total += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out);
  if (total !== targetSize || result.length !== targetSize) throw new Error('delta target size mismatch');
  return result;
}

export interface ReadPackResult {
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/** Parse a packfile (v2/v3), resolving OFS_DELTA and REF_DELTA entries. */
/**
 * Read a packfile. `lookup` resolves REF_DELTA bases that are not in the
 * pack itself (thin packs delta against objects the server already holds).
 */
export function readPack(pack: Buffer, lookup?: (oid: string) => GitObject | undefined): ReadPackResult {
  if (pack.length < 12 || pack.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new Error('not a packfile');
  }
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  const pending: Array<{ offset: number; type: ObjType | 'ofs' | 'ref'; data: Buffer; baseOffset?: number; baseOid?: string }> = [];
  let offset = 12;
  let deltaCount = 0;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = offset;
    let byte = pack[offset++] ?? 0;
    const typeId = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      byte = pack[offset++] ?? 0;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    if (typeId === 6) {
      // OFS_DELTA
      let b = pack[offset++] ?? 0;
      let ofs = b & 0x7f;
      while ((b & 0x80) !== 0) {
        b = pack[offset++] ?? 0;
        ofs = ((ofs + 1) << 7) | (b & 0x7f);
      }
      const inflated = inflatePrefix(pack, offset);
      offset += inflated.consumed;
      pending.push({ offset: entryOffset, type: 'ofs', data: inflated.data, baseOffset: entryOffset - ofs });
      deltaCount += 1;
    } else if (typeId === 7) {
      // REF_DELTA
      const baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
      const inflated = inflatePrefix(pack, offset);
      offset += inflated.consumed;
      pending.push({ offset: entryOffset, type: 'ref', data: inflated.data, baseOid });
      deltaCount += 1;
    } else {
      const type = PACK_TYPE_NAMES[typeId];
      if (type === undefined) throw new Error(`unknown pack object type ${typeId}`);
      const inflated = inflatePrefix(pack, offset);
      offset += inflated.consumed;
      if (inflated.data.length !== size) throw new Error('pack object size mismatch');
      const object: GitObject = { type, data: inflated.data };
      const oid = hashObject(type, inflated.data);
      objects.set(oid, object);
      byOffset.set(entryOffset, object);
    }
  }
  // Resolve deltas (a delta's base may itself be a delta: iterate until stable).
  let progress = true;
  while (pending.length > 0 && progress) {
    progress = false;
    for (let i = pending.length - 1; i >= 0; i -= 1) {
      const entry = pending[i]!;
      let base: GitObject | undefined;
      if (entry.type === 'ofs') base = byOffset.get(entry.baseOffset ?? -1);
      else if (entry.type === 'ref') base = objects.get(entry.baseOid ?? '') ?? lookup?.(entry.baseOid ?? '');
      if (base === undefined) continue;
      const data = applyDelta(base.data, entry.data);
      const object: GitObject = { type: base.type, data };
      const oid = hashObject(base.type, data);
      objects.set(oid, object);
      byOffset.set(entry.offset, object);
      pending.splice(i, 1);
      progress = true;
    }
  }
  if (pending.length > 0) throw new Error('pack delta base missing');
  return { objects, deltaCount };
}

// ---------------------------------------------------------------- pkt-lines

export type PktKind = 'data' | 'flush' | 'delim' | 'response-end';

export function encodePktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const length = body.length + 4;
  if (length > 65520) throw new Error('pkt-line too large');
  return Buffer.concat([Buffer.from(length.toString(16).padStart(4, '0'), 'ascii'), body]);
}

export function encodePktLines(lines: Array<string | Buffer | 'flush' | 'delim'>): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(Buffer.from('0000', 'ascii'));
    else if (line === 'delim') parts.push(Buffer.from('0001', 'ascii'));
    else parts.push(encodePktLine(line));
  }
  return Buffer.concat(parts);
}

export interface DecodedPacket {
  kind: PktKind;
  payload: Buffer;
}

/** Decode a whole buffer of pkt-lines; throws on a malformed line. */
export function decodePktLines(buf: Buffer): DecodedPacket[] {
  const packets: DecodedPacket[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('truncated pkt-line length');
    const hex = buf.subarray(offset, offset + 4).toString('ascii');
    const length = parseInt(hex, 16);
    if (Number.isNaN(length)) throw new Error('not a pkt-line');
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
    } else if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
    } else if (length === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      offset += 4;
    } else {
      if (length < 4 || offset + length > buf.length) throw new Error('pkt-line length out of range');
      packets.push({ kind: 'data', payload: buf.subarray(offset + 4, offset + length) });
      offset += length;
    }
  }
  return packets;
}

/** Split a receive-pack body into command lines and the pack bytes after the flush. */
export function splitCommands(body: Buffer): { commands: string[]; pack: Buffer } {
  let offset = 0;
  const commands: string[] = [];
  for (;;) {
    if (offset + 4 > body.length) throw new Error('truncated pkt-line in command list');
    const hex = body.subarray(offset, offset + 4).toString('ascii');
    const length = parseInt(hex, 16);
    if (Number.isNaN(length)) throw new Error('not a pkt-line');
    if (length === 0) {
      offset += 4;
      break;
    }
    if (length < 4 || offset + length > body.length) throw new Error('pkt-line length out of range');
    commands.push(body.subarray(offset + 4, offset + length).toString('utf8'));
    offset += length;
  }
  return { commands, pack: body.subarray(offset) };
}

// ---------------------------------------------------------------- forge store

export interface IssueComment {
  id: number;
  body: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: IssueComment[];
  seq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  seq: number;
}

export interface Label {
  id: number;
  name: string;
  color: string;
}

export interface Repo {
  name: string;
  owner: string;
  private: boolean;
  description: string;
  defaultBranch: string;
  dbId: number;
  createdSeq: number;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  nextNumber: number;
}

export interface FileChange {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  oldOid: string | null;
  newOid: string | null;
}

export class ForgeStore {
  objects = new Map<string, GitObject>();
  repos = new Map<string, Repo>();
  repoOrder: Repo[] = [];
  nextId = 10000;
  nextRepoSeq = 1;
  /** Parsed commit headers, cached: hot walks (ancestor checks, fetches) hit the same commits repeatedly. */
  private commitCache = new Map<string, { tree: string; parents: string[] }>();

  /** Parse a commit's tree + parents once and cache the result. */
  commitInfo(oid: string): { tree: string; parents: string[] } | undefined {
    const cached = this.commitCache.get(oid);
    if (cached !== undefined) return cached;
    const object = this.objects.get(oid);
    if (object === undefined || object.type !== 'commit') return undefined;
    const parsed = parseCommit(object.data);
    const info = { tree: parsed.tree, parents: parsed.parents };
    this.commitCache.set(oid, info);
    return info;
  }

  // ---------------------------------------------------------- object store

  putObject(type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  addObjects(objects: Map<string, GitObject>): void {
    for (const [oid, object] of objects) {
      if (!this.objects.has(oid)) this.objects.set(oid, object);
    }
  }

  // ---------------------------------------------------------- reachability

  /** All objects reachable from `roots` (commits, tags, trees, blobs). */
  /**
   * All objects reachable from roots. When `stopAtCommits` is given, a commit
   * in that set still contributes itself and its tree, but its parents are
   * NOT followed — that is how a shallow boundary works: a client that holds
   * a shallow commit has the commit and its tree, never its ancestors.
   */
  collectReachable(roots: readonly string[], stopAtCommits?: ReadonlySet<string>): { reachable: Set<string>; missing: string[] } {
    const reachable = new Set<string>();
    const missing: string[] = [];
    const stack = [...roots];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (reachable.has(oid)) continue;
      const object = this.objects.get(oid);
      if (object === undefined) {
        missing.push(oid);
        continue;
      }
      reachable.add(oid);
      if (object.type === 'commit') {
        const commit = this.commitInfo(oid) ?? parseCommit(object.data);
        stack.push(commit.tree);
        if (stopAtCommits === undefined || !stopAtCommits.has(oid)) {
          for (const parent of commit.parents) stack.push(parent);
        }
      } else if (object.type === 'tag') {
        stack.push(parseTag(object.data).object);
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) stack.push(entry.oid);
      }
    }
    return { reachable, missing };
  }

  /** Peel a tag to its target commit (or the oid itself when already a commit). */
  peelToCommit(oid: string): string | null {
    let current = oid;
    for (let i = 0; i < 10; i += 1) {
      const object = this.objects.get(current);
      if (object === undefined) return null;
      if (object.type === 'commit') return current;
      if (object.type === 'tag') {
        current = parseTag(object.data).object;
        continue;
      }
      return null;
    }
    return null;
  }

  /** Whether `ancestor` is an ancestor of (or equal to) `descendant`. */
  isAncestor(ancestor: string, descendant: string): boolean {
    const anc = this.peelToCommit(ancestor);
    const desc = this.peelToCommit(descendant);
    if (anc === null || desc === null) return false;
    if (anc === desc) return true;
    const seen = new Set<string>();
    const stack = [desc];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (oid === anc) return true;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const info = this.commitInfo(oid);
      if (info === undefined) continue;
      for (const parent of info.parents) stack.push(parent);
    }
    return false;
  }

  /** Ancestor commits of `oid` (peeled), including itself. */
  ancestorsOf(oid: string): Set<string> {
    const start = this.peelToCommit(oid);
    const out = new Set<string>();
    if (start === null) return out;
    const stack = [start];
    while (stack.length > 0) {
      const current = stack.pop()!;
      if (out.has(current)) continue;
      out.add(current);
      const info = this.commitInfo(current);
      if (info === undefined) continue;
      for (const parent of info.parents) stack.push(parent);
    }
    return out;
  }

  // ---------------------------------------------------------- trees

  /** Flatten a tree into path -> {oid, mode}. */
  flattenTree(treeOid: string): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    const walk = (oid: string, prefix: string): void => {
      const object = this.objects.get(oid);
      if (object === undefined || object.type !== 'tree') return;
      for (const entry of parseTree(object.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else out.set(path, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** Resolve `path` inside a tree: blob or subtree oid, or null. */
  resolvePath(treeOid: string, path: string): { type: 'blob' | 'tree'; oid: string } | null {
    const parts = path.split('/').filter((part) => part !== '');
    let current = treeOid;
    for (let i = 0; i < parts.length; i += 1) {
      const object = this.objects.get(current);
      if (object === undefined || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((candidate) => candidate.name === parts[i]);
      if (entry === undefined) return null;
      if (i === parts.length - 1) {
        return entry.mode === '40000' ? { type: 'tree', oid: entry.oid } : { type: 'blob', oid: entry.oid };
      }
      if (entry.mode !== '40000') return null;
      current = entry.oid;
    }
    return { type: 'tree', oid: current };
  }

  /** List a directory's entries (or null when the path is not a tree). */
  listDir(treeOid: string, path: string): TreeEntry[] | null {
    const resolved = path === '' ? { type: 'tree' as const, oid: treeOid } : this.resolvePath(treeOid, path);
    if (resolved === null || resolved.type !== 'tree') return null;
    const object = this.objects.get(resolved.oid);
    if (object === undefined || object.type !== 'tree') return null;
    return parseTree(object.data);
  }

  /** Return a new tree oid with `path` set to `blobOid` (or removed when null). */
  setPath(treeOid: string, path: string, blobOid: string | null): string {
    const parts = path.split('/').filter((part) => part !== '');
    const apply = (oid: string, depth: number): string | null => {
      const object = this.objects.get(oid);
      const entries = object !== undefined && object.type === 'tree' ? parseTree(object.data) : [];
      const name = parts[depth]!;
      const index = entries.findIndex((entry) => entry.name === name);
      if (depth === parts.length - 1) {
        if (blobOid === null) {
          if (index < 0) return oid;
          entries.splice(index, 1);
        } else if (index >= 0) {
          entries[index] = { mode: '100644', name, oid: blobOid };
        } else {
          entries.push({ mode: '100644', name, oid: blobOid });
        }
      } else {
        const child = index >= 0 && entries[index]!.mode === '40000' ? entries[index]!.oid : this.putObject('tree', buildTree([]));
        const next = apply(child, depth + 1);
        if (next === null) {
          if (index >= 0) entries.splice(index, 1);
        } else if (index >= 0) {
          entries[index] = { mode: '40000', name, oid: next };
        } else {
          entries.push({ mode: '40000', name, oid: next });
        }
      }
      if (entries.length === 0 && depth > 0) return null;
      return this.putObject('tree', buildTree(entries));
    };
    const result = apply(treeOid, 0);
    return result ?? this.putObject('tree', buildTree([]));
  }

  // ---------------------------------------------------------- repos

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== VIEWER_LOGIN) return undefined;
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo | null {
    const key = this.repoKey(VIEWER_LOGIN, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      name,
      owner: VIEWER_LOGIN,
      private: isPrivate,
      description,
      defaultBranch: DEFAULT_BRANCH,
      dbId: this.nextId++,
      createdSeq: this.nextRepoSeq++,
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
    };
    this.repos.set(key, repo);
    this.repoOrder.push(repo);
    return repo;
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(this.repoKey(repo.owner, repo.name));
    this.repoOrder = this.repoOrder.filter((entry) => entry !== repo);
  }

  listViewerRepos(): Repo[] {
    return [...this.repoOrder].sort((a, b) => a.createdSeq - b.createdSeq);
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  /** Resolve a branch name or a commit oid to a commit oid. */
  resolveRef(repo: Repo, ref: string): string | undefined {
    const direct = repo.refs.get(`refs/heads/${ref}`);
    if (direct !== undefined) return direct;
    if (/^[0-9a-f]{40}$/.test(ref)) {
      const object = this.objects.get(ref);
      if (object !== undefined && object.type === 'commit') return ref;
      const peeled = this.peelToCommit(ref);
      if (peeled !== null) return peeled;
    }
    return undefined;
  }

  /** Commits reachable from `ref`, newest first (pre-order walk, deduped). */
  listCommits(repo: Repo, ref: string): string[] {
    const tip = this.resolveRef(repo, ref);
    if (tip === undefined) return [];
    const out: string[] = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = this.objects.get(oid);
      if (object === undefined || object.type !== 'commit') continue;
      out.push(oid);
      const parents = parseCommit(object.data).parents;
      for (let i = parents.length - 1; i >= 0; i -= 1) stack.push(parents[i]!);
    }
    return out;
  }

  // ---------------------------------------------------------- contents

  /** Create a commit on `branch` (default branch when undefined) writing one file. */
  commitFile(
    repo: Repo,
    branch: string | undefined,
    path: string,
    content: Buffer,
    message: string,
    expectedSha?: string,
  ): { status: 'created' | 'updated'; commitSha: string; blobSha: string } | { status: 'error'; code: number; message: string } {
    const branchName = branch ?? repo.defaultBranch;
    const ref = `refs/heads/${branchName}`;
    const tip = repo.refs.get(ref);
    const blobSha = this.putObject('blob', content);
    let baseTree = '';
    let parent: string[] = [];
    let fileExisted = false;
    let isNew = false;
    if (tip === undefined) {
      if (branch !== undefined) return { status: 'error', code: 404, message: 'Branch not found' };
      baseTree = this.putObject('tree', buildTree([]));
      isNew = true;
    } else {
      const commit = parseCommit(this.objects.get(tip)!.data);
      baseTree = commit.tree;
      parent = [tip];
      const existing = this.resolvePath(baseTree, path);
      if (existing !== null && existing.type === 'blob') {
        fileExisted = true;
        if (expectedSha === undefined) return { status: 'error', code: 422, message: 'sha is required to update a file' };
        if (existing.oid.toLowerCase() !== expectedSha.toLowerCase()) {
          return { status: 'error', code: 409, message: 'sha does not match the current file' };
        }
      } else if (existing !== null) {
        return { status: 'error', code: 422, message: 'path is a directory' };
      } else if (expectedSha !== undefined) {
        return { status: 'error', code: 422, message: 'sha was supplied for a file that does not exist' };
      }
    }
    const tree = this.setPath(baseTree, path, blobSha);
    const commitSha = this.putObject('commit', buildCommit(tree, parent, message));
    repo.refs.set(ref, commitSha);
    return { status: isNew || !fileExisted ? 'created' : 'updated', commitSha, blobSha };
  }

  deleteFile(
    repo: Repo,
    branch: string | undefined,
    path: string,
    message: string,
    expectedSha?: string,
  ): { commitSha: string } | { status: 'error'; code: number; message: string } {
    const branchName = branch ?? repo.defaultBranch;
    const ref = `refs/heads/${branchName}`;
    const tip = repo.refs.get(ref);
    if (tip === undefined) return { status: 'error', code: 404, message: 'Not Found' };
    const commit = parseCommit(this.objects.get(tip)!.data);
    const existing = this.resolvePath(commit.tree, path);
    if (existing === null || existing.type !== 'blob') return { status: 'error', code: 404, message: 'Not Found' };
    if (expectedSha !== undefined && existing.oid.toLowerCase() !== expectedSha.toLowerCase()) {
      return { status: 'error', code: 409, message: 'sha does not match the current file' };
    }
    const tree = this.setPath(commit.tree, path, null);
    const commitSha = this.putObject('commit', buildCommit(tree, [tip], message));
    repo.refs.set(ref, commitSha);
    return { commitSha };
  }

  // ---------------------------------------------------------- issues

  createIssue(repo: Repo, title: string, body: string, labelNames: string[]): Issue | { status: 'error'; code: number; message: string } {
    for (const name of labelNames) {
      if (!repo.labels.has(name)) return { status: 'error', code: 422, message: `Label '${name}' does not exist` };
    }
    const number = repo.nextNumber++;
    const issue: Issue = {
      id: this.nextId++,
      number,
      title,
      body: body ?? '',
      state: 'open',
      labels: [...labelNames],
      comments: [],
      seq: number,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  // ---------------------------------------------------------- pulls

  createPull(repo: Repo, title: string, body: string, head: string, base: string): Pull | { status: 'error'; code: number; message: string } {
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (headSha === undefined) return { status: 'error', code: 422, message: `Head branch '${head}' does not exist` };
    if (baseSha === undefined) return { status: 'error', code: 422, message: `Base branch '${base}' does not exist` };
    for (const pull of repo.pulls.values()) {
      if (pull.state === 'open' && pull.headRef === head && pull.baseRef === base) {
        return { status: 'error', code: 422, message: 'A pull request is already open for this head and base' };
      }
    }
    const number = repo.nextNumber++;
    const pull: Pull = {
      id: this.nextId++,
      number,
      title,
      body: body ?? '',
      state: 'open',
      headRef: head,
      baseRef: base,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeSha: null,
      seq: number,
    };
    repo.pulls.set(number, pull);
    return pull;
  }

  /** Files changed between the pull's base snapshot and its head. */
  pullFiles(repo: Repo, pull: Pull): FileChange[] {
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    const baseTreeOid = baseTip === undefined ? '' : parseCommit(this.objects.get(baseTip)!.data).tree;
    const headTreeOid = parseCommit(this.objects.get(pull.headSha)!.data).tree;
    const baseFlat = baseTreeOid === '' ? new Map() : this.flattenTree(baseTreeOid);
    const headFlat = this.flattenTree(headTreeOid);
    // The diff is against the base as it was when the pull was opened.
    const createdTreeOid = parseCommit(this.objects.get(pull.baseSha)!.data).tree;
    const createdFlat = this.flattenTree(createdTreeOid);
    return diffFlat(this, createdFlat, headFlat);
  }

  /** Whether the pull can merge cleanly (no file changed both ways with different bytes). */
  pullMergeable(repo: Repo, pull: Pull): boolean {
    if (pull.state !== 'open') return false;
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (baseTip === undefined) return false;
    const createdTreeOid = parseCommit(this.objects.get(pull.baseSha)!.data).tree;
    const createdFlat = this.flattenTree(createdTreeOid);
    const headTreeOid = parseCommit(this.objects.get(pull.headSha)!.data).tree;
    const headFlat = this.flattenTree(headTreeOid);
    const currentTreeOid = parseCommit(this.objects.get(baseTip)!.data).tree;
    const currentFlat = this.flattenTree(currentTreeOid);
    const paths = new Set([...headFlat.keys(), ...currentFlat.keys(), ...createdFlat.keys()]);
    for (const path of paths) {
      const from = createdFlat.get(path)?.oid ?? null;
      const head = headFlat.get(path)?.oid ?? null;
      const current = currentFlat.get(path)?.oid ?? null;
      if (head !== from && current !== from && head !== current) return false;
    }
    return true;
  }

  /** Squash-merge a pull: one commit on the base branch with the head's tree. */
  mergePull(repo: Repo, pull: Pull): { sha: string } | { status: 'error'; code: number; message: string } {
    if (pull.state !== 'open') return { status: 'error', code: 404, message: 'Pull request is not open' };
    if (!this.pullMergeable(repo, pull)) return { status: 'error', code: 405, message: 'Pull request is conflicted' };
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (baseTip === undefined) return { status: 'error', code: 404, message: 'Base branch not found' };
    const headTreeOid = parseCommit(this.objects.get(pull.headSha)!.data).tree;
    const message = `Merge pull request #${pull.number} from ${pull.headRef}`;
    const commitSha = this.putObject('commit', buildCommit(headTreeOid, [baseTip], message));
    repo.refs.set(`refs/heads/${pull.baseRef}`, commitSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = FIXED_ISO;
    pull.mergeSha = commitSha;
    return { sha: commitSha };
  }
}

// ---------------------------------------------------------- tree diff

function countLines(data: Buffer): string[] {
  const text = data.toString('utf8');
  if (text === '') return [];
  const lines = text.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines;
}

export function diffFlat(
  store: ForgeStore,
  baseFlat: Map<string, { oid: string; mode: string }>,
  headFlat: Map<string, { oid: string; mode: string }>,
): FileChange[] {
  const out: FileChange[] = [];
  const paths = new Set([...baseFlat.keys(), ...headFlat.keys()]);
  for (const path of [...paths].sort()) {
    const base = baseFlat.get(path);
    const head = headFlat.get(path);
    if (base !== undefined && head !== undefined && base.oid === head.oid) continue;
    if (base === undefined && head !== undefined) {
      const lines = countLines(store.objects.get(head.oid)?.data ?? Buffer.alloc(0));
      out.push({ filename: path, status: 'added', additions: lines.length, deletions: 0, changes: lines.length, oldOid: null, newOid: head.oid });
    } else if (base !== undefined && head === undefined) {
      const lines = countLines(store.objects.get(base.oid)?.data ?? Buffer.alloc(0));
      out.push({ filename: path, status: 'removed', additions: 0, deletions: lines.length, changes: lines.length, oldOid: base.oid, newOid: null });
    } else if (base !== undefined && head !== undefined) {
      const oldLines = countLines(store.objects.get(base.oid)?.data ?? Buffer.alloc(0));
      const newLines = countLines(store.objects.get(head.oid)?.data ?? Buffer.alloc(0));
      const oldCount = new Map<string, number>();
      for (const line of oldLines) oldCount.set(line, (oldCount.get(line) ?? 0) + 1);
      let additions = 0;
      for (const line of newLines) {
        const remaining = oldCount.get(line) ?? 0;
        if (remaining > 0) oldCount.set(line, remaining - 1);
        else additions += 1;
      }
      let deletions = 0;
      for (const remaining of oldCount.values()) deletions += remaining;
      if (additions === 0 && deletions === 0) {
        additions = 1;
      }
      out.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions, oldOid: base.oid, newOid: head.oid });
    }
  }
  return out;
}

// ---------------------------------------------------------- global ids

/** Opaque, stable GraphQL global ids: base64 of a namespaced key, never the raw number. */
export function globalId(kind: string, key: string): string {
  return Buffer.from(`gf:${kind}:${key}`, 'utf8').toString('base64');
}

export function parseGlobalId(id: string): { kind: string; key: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    if (!text.startsWith('gf:')) return null;
    const rest = text.slice(3);
    const colon = rest.indexOf(':');
    if (colon < 0) return null;
    return { kind: rest.slice(0, colon), key: rest.slice(colon + 1) };
  } catch {
    return null;
  }
}
