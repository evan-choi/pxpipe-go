import { readFileSync } from 'node:fs';
import { transformOpenAIChatCompletions, transformOpenAIResponses } from '../../pxpipe/src/core/openai.js';
async function bench(name: string, fn: (b: Uint8Array) => Promise<unknown>, body: Uint8Array, n: number) {
  await fn(body); await fn(body);
  const t0 = performance.now();
  for (let i = 0; i < n; i++) await fn(body);
  const dt = (performance.now() - t0) / n;
  console.log(`${name}: ${dt.toFixed(1)} ms/op`);
}
const chat = readFileSync('../pxpipe-go/testdata/openai/chat-big-slab/input.json');
const resp = readFileSync('../pxpipe-go/testdata/openai/responses-codex-pairs/input.json');
async function main() {
  await bench('chat-big-slab', (b) => transformOpenAIChatCompletions(b), chat, 5);
  await bench('responses-codex-pairs', (b) => transformOpenAIResponses(b), resp, 5);
}
main();
