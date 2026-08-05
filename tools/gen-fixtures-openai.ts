/**
 * Golden fixture generator for the Go port — OpenAI Chat Completions +
 * Responses paths.
 *
 * Run from the pxpipe (TS) repo root:
 *   pnpm exec tsx ../pxpipe-go/tools/gen-fixtures-openai.ts
 *
 * PNG bytes are NOT expected to match byte-for-byte (different deflate
 * implementations); the Go tests decode both sides and compare pixels.
 */
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  transformOpenAIChatCompletions,
  transformOpenAIResponses,
} from '../../pxpipe/src/core/openai.js';
import type { TransformOptions } from '../../pxpipe/src/core/transform.js';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', 'testdata');

function outDir(...parts: string[]): string {
  const d = join(ROOT, ...parts);
  mkdirSync(d, { recursive: true });
  return d;
}

function lcg(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 0x100000000;
  };
}

const WORDS = ['alpha', 'beta', 'gamma', 'delta', 'proxy', 'render', 'token', 'image', 'const', 'return', 'await', 'buffer', 'stream', 'cache', 'block', 'chunk'];

function fakeCode(lines: number, seed: number): string {
  const rnd = lcg(seed);
  const out: string[] = [];
  for (let i = 0; i < lines; i++) {
    const indent = '  '.repeat(Math.floor(rnd() * 4));
    const w = () => WORDS[Math.floor(rnd() * WORDS.length)];
    out.push(`${indent}const ${w()}_${i} = ${w()}(${w()}, ${Math.floor(rnd() * 10000)}); // ${w()} ${w()}`);
  }
  return out.join('\n');
}

function fakeLog(lines: number, seed: number): string {
  const rnd = lcg(seed);
  const out: string[] = [];
  for (let i = 0; i < lines; i++) {
    out.push(`2026-08-05T10:${String(i % 60).padStart(2, '0')}:00Z [info] worker=${Math.floor(rnd() * 8)} req=req_${(rnd() * 1e9).toFixed(0)} status=${rnd() > 0.1 ? 200 : 500} dur=${(rnd() * 900).toFixed(1)}ms`);
  }
  return out.join('\n');
}

function fakeProse(paragraphs: number, seed: number): string {
  const rnd = lcg(seed);
  const out: string[] = [];
  for (let i = 0; i < paragraphs; i++) {
    const sentences: string[] = [];
    for (let j = 0; j < 4 + Math.floor(rnd() * 4); j++) {
      const w = () => WORDS[Math.floor(rnd() * WORDS.length)];
      sentences.push(`The ${w()} ${w()} must ${w()} every ${w()} before the ${w()} completes step ${Math.floor(rnd() * 100)}.`);
    }
    out.push(sentences.join(' '));
  }
  return out.join('\n\n');
}

// ---------------------------------------------------------------------------
// Shared request material
// ---------------------------------------------------------------------------

const BIG_SYSTEM = [
  '# Agent Operating Manual',
  fakeProse(30, 11),
  '## Coding rules',
  fakeCode(120, 12),
  '## Escalation log format',
  fakeLog(60, 13),
].join('\n\n');

const SMALL_SYSTEM = 'You are terse. Answer in one sentence.';

function chatTools(n: number): unknown[] {
  const tools: unknown[] = [];
  for (let i = 0; i < n; i++) {
    tools.push({
      type: 'function',
      function: {
        name: `tool_${i}`,
        description: `Tool number ${i}. ${fakeProse(2, 100 + i).replace(/\n/g, ' ')}`,
        parameters: {
          type: 'object',
          properties: {
            path: { type: 'string', description: `Absolute file path for tool ${i}.` },
            limit: { type: 'number', description: 'Maximum entries to return.' },
            flags: { type: 'array', items: { type: 'string' }, description: 'Optional flag list.' },
          },
          required: ['path'],
        },
      },
    });
  }
  return tools;
}

function flatTools(n: number): unknown[] {
  const tools: unknown[] = [];
  for (let i = 0; i < n; i++) {
    tools.push({
      type: 'function',
      name: `tool_${i}`,
      description: `Tool number ${i}. ${fakeProse(2, 100 + i).replace(/\n/g, ' ')}`,
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string', description: `Absolute file path for tool ${i}.` },
          limit: { type: 'number', description: 'Maximum entries to return.' },
        },
        required: ['path'],
      },
    });
  }
  return tools;
}

function chatHistory(turns: number): unknown[] {
  const msgs: unknown[] = [];
  for (let i = 0; i < turns; i++) {
    msgs.push({ role: 'user', content: `Turn ${i}: inspect module ${i}.\n${fakeCode(6, 300 + i)}` });
    msgs.push({
      role: 'assistant',
      content: `Working on turn ${i}.`,
      tool_calls: [
        {
          id: `call_${i}_a`,
          type: 'function',
          function: { name: 'tool_1', arguments: JSON.stringify({ path: `/src/mod${i}.ts` }) },
        },
      ],
    });
    msgs.push({ role: 'tool', tool_call_id: `call_${i}_a`, content: fakeLog(40, 400 + i) });
    msgs.push({ role: 'assistant', content: `Turn ${i} done. Key hash: deadbeef${i}cafe.` });
  }
  msgs.push({ role: 'user', content: 'Final question: summarize everything above in one paragraph.' });
  return msgs;
}

function responsesItems(rounds: number, opts: { parallel?: boolean; interleave?: boolean } = {}): unknown[] {
  const items: unknown[] = [];
  items.push({ role: 'user', content: 'Start the audit of the whole repository and keep me posted.' });
  for (let i = 0; i < rounds; i++) {
    if (opts.interleave && i > 0) {
      items.push({
        role: 'assistant',
        content: [{ type: 'output_text', text: `Round ${i - 1} reviewed. Proceeding to round ${i}.` }],
      });
    }
    if (opts.parallel && i % 3 === 0) {
      items.push({ type: 'function_call', call_id: `call_${i}_a`, name: 'tool_0', arguments: JSON.stringify({ path: `/src/a${i}.ts` }) });
      items.push({ type: 'function_call', call_id: `call_${i}_b`, name: 'tool_1', arguments: JSON.stringify({ path: `/src/b${i}.ts` }) });
      items.push({ type: 'function_call_output', call_id: `call_${i}_a`, output: fakeLog(30, 500 + i) });
      items.push({ type: 'function_call_output', call_id: `call_${i}_b`, output: fakeLog(30, 600 + i) });
    } else {
      items.push({ type: 'function_call', call_id: `call_${i}`, name: 'tool_0', arguments: JSON.stringify({ path: `/src/mod${i}.ts`, limit: i }) });
      items.push({ type: 'function_call_output', call_id: `call_${i}`, output: fakeLog(40, 700 + i) });
    }
  }
  items.push({ role: 'user', content: 'Now summarize the audit results with exact file names.' });
  return items;
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

interface OpenAICase {
  name: string;
  endpoint: 'chat' | 'responses';
  body: unknown;
  opts: TransformOptions;
}

const CASES: OpenAICase[] = [
  {
    name: 'chat-big-slab',
    endpoint: 'chat',
    body: {
      model: 'gpt-5.4',
      messages: [
        { role: 'system', content: BIG_SYSTEM },
        { role: 'developer', content: '## Router rules\n' + fakeProse(8, 21) },
        ...chatHistory(2),
      ],
      tools: chatTools(12),
      stream: true,
    },
    opts: {},
  },
  {
    name: 'chat-below-min',
    endpoint: 'chat',
    body: {
      model: 'gpt-5.4',
      messages: [
        { role: 'system', content: SMALL_SYSTEM },
        { role: 'user', content: 'hi' },
      ],
    },
    opts: {},
  },
  {
    name: 'chat-history-long',
    endpoint: 'chat',
    body: {
      model: 'gpt-5.4',
      messages: [
        { role: 'system', content: SMALL_SYSTEM },
        ...chatHistory(30),
      ],
    },
    opts: {},
  },
  {
    name: 'chat-4o-tile',
    endpoint: 'chat',
    body: {
      model: 'gpt-4o',
      messages: [
        { role: 'system', content: BIG_SYSTEM },
        { role: 'user', content: 'Apply the manual to this repo.' },
      ],
      tools: chatTools(6),
    },
    opts: {},
  },
  {
    name: 'chat-compress-disabled',
    endpoint: 'chat',
    body: {
      model: 'gpt-5.4',
      messages: [
        { role: 'system', content: BIG_SYSTEM },
        { role: 'user', content: 'hello' },
      ],
    },
    opts: { compress: false },
  },
  {
    name: 'chat-parts-content',
    endpoint: 'chat',
    body: {
      model: 'gpt-5.4',
      messages: [
        { role: 'system', content: [{ type: 'text', text: BIG_SYSTEM }] },
        {
          role: 'user',
          content: [
            { type: 'text', text: 'What does the manual say about escalation?' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,aGk=' } },
          ],
        },
      ],
      tools: chatTools(4),
    },
    opts: {},
  },
  {
    name: 'responses-codex-pairs',
    endpoint: 'responses',
    body: {
      model: 'gpt-5',
      instructions: BIG_SYSTEM,
      input: responsesItems(24),
      tools: flatTools(8),
    },
    opts: {},
  },
  {
    name: 'responses-sol-mixed',
    endpoint: 'responses',
    body: {
      model: 'gpt-5.6-sol',
      instructions: BIG_SYSTEM,
      input: responsesItems(24, { interleave: true }),
      tools: flatTools(8),
    },
    opts: {},
  },
  {
    name: 'responses-parallel-rounds',
    endpoint: 'responses',
    body: {
      model: 'gpt-5',
      instructions: SMALL_SYSTEM,
      input: responsesItems(18, { parallel: true }),
    },
    opts: {},
  },
  {
    name: 'responses-string-input',
    endpoint: 'responses',
    body: {
      model: 'gpt-5.4',
      instructions: BIG_SYSTEM,
      input: 'Summarize the operating manual in three bullets.',
      tools: flatTools(4),
    },
    opts: {},
  },
  {
    name: 'responses-grok-mpix',
    endpoint: 'responses',
    body: {
      model: 'grok-4.5',
      instructions: BIG_SYSTEM,
      input: responsesItems(12),
      tools: flatTools(6),
    },
    opts: {},
  },
  {
    name: 'responses-below-min',
    endpoint: 'responses',
    body: {
      model: 'gpt-5',
      instructions: SMALL_SYSTEM,
      input: [{ role: 'user', content: 'ping' }],
    },
    opts: {},
  },
  {
    name: 'responses-barriers',
    endpoint: 'responses',
    body: {
      model: 'gpt-5.6-sol',
      instructions: SMALL_SYSTEM,
      input: [
        { role: 'user', content: 'Begin.' },
        ...responsesItems(10).slice(1, -1),
        { type: 'reasoning', encrypted_content: 'opaque-blob-'.repeat(50) },
        ...responsesItems(10, { interleave: true }).slice(1, -1),
        { type: 'item_reference', id: 'ref_1' },
        { role: 'user', content: 'Final: list the audited files.' },
      ],
    },
    opts: {},
  },
  {
    name: 'responses-system-items',
    endpoint: 'responses',
    body: {
      model: 'gpt-5.4',
      input: [
        { role: 'system', content: BIG_SYSTEM },
        { role: 'developer', content: [{ type: 'input_text', text: '## Precedence\n' + fakeProse(6, 31) }] },
        { role: 'user', content: 'Do the thing.' },
      ],
      tools: flatTools(4),
    },
    opts: {},
  },
  {
    name: 'chat-charspertoken-3',
    endpoint: 'chat',
    body: {
      model: 'gpt-4.1',
      messages: [
        { role: 'system', content: BIG_SYSTEM },
        { role: 'user', content: 'go' },
      ],
    },
    opts: { charsPerToken: 3 },
  },
];

async function main() {
  rmSync(join(ROOT, 'openai'), { recursive: true, force: true });
  for (const c of CASES) {
    const dir = outDir('openai', c.name);
    const bodyBytes = new TextEncoder().encode(JSON.stringify(c.body));
    writeFileSync(join(dir, 'input.json'), bodyBytes);
    writeFileSync(join(dir, 'opts.json'), JSON.stringify({ endpoint: c.endpoint, ...c.opts }, null, 2));
    const fn = c.endpoint === 'chat' ? transformOpenAIChatCompletions : transformOpenAIResponses;
    const { body, info } = await fn(bodyBytes, c.opts);
    writeFileSync(join(dir, 'output.json'), body);
    const { imagePngs: _p, firstImagePng: _f, ...serializableInfo } = info as Record<string, unknown>;
    writeFileSync(join(dir, 'info.json'), JSON.stringify(serializableInfo, null, 2));
    console.log(`openai/${c.name}: compressed=${info.compressed} reason=${info.reason ?? ''} images=${info.imageCount} histReason=${info.historyReason ?? ''}`);
  }
  console.log('done');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
