// Entry point. The forge itself is a statically linked Go binary committed
// beside this file as `forge`; the harness starts repositories with
// `node main.ts`, so this launcher stages the binary somewhere executable
// (the repository mount is read-only and a committed exec bit is not
// guaranteed), runs it with the environment passed through, and exits with
// the binary's exit code.
import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const source = join(here, 'forge');
const staged = '/tmp/git-forge-binary';

let binary: string = source;
try {
  copyFileSync(source, staged);
  chmodSync(staged, 0o755);
  binary = staged;
} catch {
  // Staging failed (no writable /tmp): run the committed file in place.
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
child.on('error', (err: Error) => {
  console.error(`failed to start the forge binary: ${err.message}`);
  process.exit(1);
});
// If the harness stops this launcher, the binary must not outlive it.
for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}
child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
