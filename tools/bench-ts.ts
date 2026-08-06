/**
 * TS-side counterpart of the Go benchmarks: same fixtures, same operations.
 * Run from the pxpipe submodule root:
 *   pnpm exec tsx ../tools/bench-ts.ts
 */
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { transformRequest } from '../pxpipe/src/core/transform.js';
import { renderTextToImages } from '../pxpipe/src/core/library.js';
import {
  transformOpenAIChatCompletions,
  transformOpenAIResponses,
} from '../pxpipe/src/core/openai.js';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', 'testdata');

async function bench(name: string, warmup: number, iters: number, fn: () => Promise<void>) {
  for (let i = 0; i < warmup; i++) await fn();
  const times: number[] = [];
  for (let i = 0; i < iters; i++) {
    const t0 = performance.now();
    await fn();
    times.push(performance.now() - t0);
  }
  times.sort((a, b) => a - b);
  const mean = times.reduce((a, b) => a + b, 0) / times.length;
  console.log(
    `${name}: mean=${mean.toFixed(1)}ms min=${times[0]!.toFixed(1)}ms p50=${times[Math.floor(times.length / 2)]!.toFixed(1)}ms max=${times[times.length - 1]!.toFixed(1)}ms (n=${iters})`,
  );
}

async function main() {
  const body = new Uint8Array(readFileSync(join(ROOT, 'transform', 'big-claude-code', 'input.json')));
  await bench('TransformBigClaudeCode', 2, 3, async () => {
    const { info } = await transformRequest(body, { model: 'claude-fable-5' });
    if (!info.compressed) throw new Error('expected compression');
  });

  const text = readFileSync(join(ROOT, 'render', 'multi-page', 'input.txt'), 'utf8');
  await bench('RenderDensePage', 2, 3, async () => {
    await renderTextToImages(text, { reflow: true });
  });

  const chat = new Uint8Array(readFileSync(join(ROOT, 'openai', 'chat-big-slab', 'input.json')));
  await bench('TransformOpenAIChat', 2, 3, async () => {
    const { info } = await transformOpenAIChatCompletions(chat);
    if (!info.compressed) throw new Error('expected compression');
  });

  const responses = new Uint8Array(
    readFileSync(join(ROOT, 'openai', 'responses-codex-pairs', 'input.json')),
  );
  await bench('TransformOpenAIResponses', 2, 3, async () => {
    const { info } = await transformOpenAIResponses(responses);
    if (!info.compressed) throw new Error('expected compression');
  });
}

main();
