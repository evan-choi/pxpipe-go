/**
 * Golden fixture generator for the Go port.
 *
 * Runs the TS reference implementation from the pxpipe submodule and dumps
 * inputs/outputs under testdata so the Go tests can diff against them.
 *
 * Run from the pxpipe submodule root:
 *   bun run ../tools/gen-fixtures.ts
 *
 * PNG bytes are NOT expected to match byte-for-byte (different deflate
 * implementations); the Go tests decode both sides and compare pixels.
 */
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderTextToImages, type RenderTextToImagesOptions } from '../pxpipe/src/core/library.js';
import { transformRequest, type TransformOptions } from '../pxpipe/src/core/transform.js';
import { synthesizeText, PRODUCTION_SLAB_161K, BELOW_MIN_CHARS_TINY } from '../pxpipe/tests/fixtures/real-shapes.js';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', 'testdata');

function outDir(...parts: string[]): string {
  const d = join(ROOT, ...parts);
  mkdirSync(d, { recursive: true });
  return d;
}

// ---------------------------------------------------------------------------
// Deterministic pseudo-text helpers
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Render fixtures
// ---------------------------------------------------------------------------

interface RenderCase {
  name: string;
  text: string;
  opts: RenderTextToImagesOptions;
}

const KOREAN = [
  '프록시는 요청 본문을 PNG 이미지로 변환합니다.',
  '한글과 English와 日本語가 섞인 monospace 렌더링 테스트.',
  '전각문자：ＡＢＣ　１２３（테스트）',
  '```go',
  'func main() { fmt.Println("안녕, 世界") }',
  '```',
].join('\n');

const RENDER_CASES: RenderCase[] = [
  { name: 'ascii-basic', text: fakeCode(40, 1), opts: {} },
  { name: 'ascii-reflow', text: fakeCode(120, 2), opts: { reflow: true } },
  { name: 'cjk-korean', text: KOREAN, opts: {} },
  { name: 'cjk-korean-reflow', text: (KOREAN + '\n').repeat(30), opts: { reflow: true } },
  { name: 'shrink-off-cols120', text: fakeCode(20, 3), opts: { shrink: false, cols: 120 } },
  { name: 'multi-page', text: fakeLog(3000, 4), opts: { reflow: true } },
  { name: 'font-jbmono10', text: fakeCode(25, 5), opts: { style: { font: 'jetbrains-mono-10', aa: true } } },
  { name: 'font-jbmono12', text: fakeCode(25, 5), opts: { style: { font: 'jetbrains-mono-12', aa: true } } },
  { name: 'font-jbmono14', text: fakeCode(25, 5), opts: { style: { font: 'jetbrains-mono-14', aa: true } } },
  { name: 'font-spleen-noaa', text: fakeCode(25, 5), opts: { style: { aa: false } } },
  { name: 'style-grid-marker-red', text: fakeCode(60, 6), opts: { reflow: true, style: { aa: true, grid: true, gridCols: 8, markerRed: true, markerScale: 2 } } },
  { name: 'style-dilate-papergray', text: fakeCode(30, 7), opts: { style: { aa: false, inkDilate: 1, inkDilateAxis: 'y', paperGray: 236 } } },
  { name: 'tabs-and-controls', text: 'col1\tcol2\tcol3\n\tindented\ttab\nctrl:\x07bell\x00nul end', opts: {} },
  { name: 'missing-glyphs-emoji', text: 'emoji: 🚀🔥 and rare: \u{10348}\u2603 done', opts: {} },
  { name: 'empty-lines-runs', text: 'a\n\n\n\n\nb\n   \n\t\nc   \n', opts: { reflow: true } },
  { name: 'maxchars-small-pages', text: fakeCode(200, 8), opts: { maxCharsPerImage: 2000 } },
  { name: 'maxheight-clamp', text: fakeCode(300, 9), opts: { maxHeightPx: 240 } },
];

async function genRender() {
  rmSync(join(ROOT, 'render'), { recursive: true, force: true });
  for (const c of RENDER_CASES) {
    const dir = outDir('render', c.name);
    writeFileSync(join(dir, 'input.txt'), c.text);
    writeFileSync(join(dir, 'opts.json'), JSON.stringify(c.opts, null, 2));
    const res = await renderTextToImages(c.text, c.opts);
    const pages: unknown[] = [];
    res.pages.forEach((p, i) => {
      const f = `page-${i}.png`;
      writeFileSync(join(dir, f), p.png);
      pages.push({ file: f, width: p.width, height: p.height });
    });
    writeFileSync(
      join(dir, 'result.json'),
      JSON.stringify({ pages, droppedChars: res.droppedChars, pixels: res.pixels }, null, 2),
    );
    console.log(`render/${c.name}: ${res.pages.length} page(s)`);
  }
}

// ---------------------------------------------------------------------------
// Transform fixtures
// ---------------------------------------------------------------------------

function bigTools(n: number): unknown[] {
  const tools: unknown[] = [];
  for (let i = 0; i < n; i++) {
    tools.push({
      name: `tool_${i}`,
      description: `Tool number ${i}. ${fakeCode(6, 100 + i).replace(/\n/g, ' ')}\nUsage notes:\n${fakeLog(4, 200 + i)}`,
      input_schema: {
        type: 'object',
        properties: {
          path: { type: 'string', description: `The file path argument for tool ${i}; must be absolute.` },
          limit: { type: 'number', description: 'Maximum entries to return.' },
          flags: { type: 'array', items: { type: 'string' }, description: 'Optional flag list controlling behavior.' },
        },
        required: ['path'],
      },
    });
  }
  return tools;
}

function bigHistory(turns: number): unknown[] {
  const msgs: unknown[] = [];
  for (let i = 0; i < turns; i++) {
    msgs.push({ role: 'user', content: `Turn ${i}: please inspect module ${i} and summarize.\n${fakeCode(8, 300 + i)}` });
    msgs.push({
      role: 'assistant',
      content: [
        { type: 'text', text: `Working on turn ${i}.` },
        { type: 'tool_use', id: `toolu_${i}_a`, name: 'tool_1', input: { path: `/src/mod${i}.ts` } },
      ],
    });
    msgs.push({
      role: 'user',
      content: [
        { type: 'tool_result', tool_use_id: `toolu_${i}_a`, content: fakeLog(60, 400 + i) },
      ],
    });
    msgs.push({ role: 'assistant', content: `Turn ${i} done: module ${i} looks healthy. Key hash: deadbeef${i}cafe.` });
  }
  msgs.push({ role: 'user', content: 'Final question: summarize everything above in one paragraph.' });
  return msgs;
}

interface TransformCase {
  name: string;
  body: unknown;
  opts: TransformOptions;
}

const SYSTEM_SLAB = synthesizeText(PRODUCTION_SLAB_161K).slice(0, 60000);

const CLAUDE_CODE_REQ = {
  model: 'claude-fable-5',
  max_tokens: 8192,
  system: [
    { type: 'text', text: 'You are Claude Code, an agentic coding assistant.' },
    { type: 'text', text: SYSTEM_SLAB, cache_control: { type: 'ephemeral' } },
  ],
  tools: bigTools(24),
  messages: bigHistory(10),
  metadata: { user_id: 'user_fixture_1' },
  stream: true,
};

const TRANSFORM_CASES: TransformCase[] = [
  { name: 'big-claude-code', body: CLAUDE_CODE_REQ, opts: { model: 'claude-fable-5' } },
  {
    name: 'below-min-tiny',
    body: {
      model: 'claude-fable-5',
      max_tokens: 1024,
      system: 'You are terse.',
      messages: [{ role: 'user', content: synthesizeText(BELOW_MIN_CHARS_TINY).slice(0, 800) }],
    },
    opts: { model: 'claude-fable-5' },
  },
  {
    name: 'compress-disabled',
    body: CLAUDE_CODE_REQ,
    opts: { model: 'claude-fable-5', compress: false } as TransformOptions,
  },
  {
    name: 'string-system-medium',
    body: {
      model: 'claude-fable-5',
      max_tokens: 4096,
      system: synthesizeText(PRODUCTION_SLAB_161K).slice(0, 45000),
      messages: [
        { role: 'user', content: 'Start.' },
        { role: 'assistant', content: 'OK.' },
        { role: 'user', content: 'Continue with the plan described in the system prompt.' },
      ],
    },
    opts: { model: 'claude-fable-5' },
  },
  {
    name: 'giant-tool-result-paging',
    body: {
      model: 'claude-fable-5',
      max_tokens: 8192,
      system: [{ type: 'text', text: SYSTEM_SLAB, cache_control: { type: 'ephemeral' } }],
      tools: bigTools(4),
      messages: [
        { role: 'user', content: 'Read the giant log.' },
        {
          role: 'assistant',
          content: [{ type: 'tool_use', id: 'toolu_giant', name: 'tool_0', input: { path: '/var/log/giant.log' } }],
        },
        { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_giant', content: fakeLog(30000, 42) }] },
        { role: 'assistant', content: 'That is a lot of log.' },
        { role: 'user', content: 'Now count the 500s.' },
      ],
    },
    opts: { model: 'claude-fable-5' },
  },
  {
    name: 'keep-sharp-block',
    body: CLAUDE_CODE_REQ,
    // Serialized to opts.json as { keepSharpRule: 'contains:deadbeef' };
    // the Go test reconstructs the same predicate.
    opts: {
      model: 'claude-fable-5',
      keepSharp: ((b: { text: string }) => b.text.includes('deadbeef')) as never,
    },
  },
  {
    name: 'emit-recoverable',
    body: CLAUDE_CODE_REQ,
    opts: { model: 'claude-fable-5', emitRecoverable: true },
  },
  {
    name: 'chars-per-token-4',
    body: CLAUDE_CODE_REQ,
    opts: { model: 'claude-fable-5', charsPerToken: 4 },
  },
  {
    name: 'pin-command',
    body: {
      model: 'claude-fable-5',
      max_tokens: 4096,
      system: 'You are terse.',
      messages: [
        { role: 'user', content: '@pxpipe pin /src/core/render.ts' },
        { role: 'assistant', content: 'noted' },
        { role: 'user', content: 'What did I pin?' },
      ],
    },
    opts: { model: 'claude-fable-5' },
  },
];

async function genTransform() {
  rmSync(join(ROOT, 'transform'), { recursive: true, force: true });
  for (const c of TRANSFORM_CASES) {
    const dir = outDir('transform', c.name);
    const bodyBytes = new TextEncoder().encode(JSON.stringify(c.body));
    writeFileSync(join(dir, 'input.json'), bodyBytes);
    const serializableOpts: Record<string, unknown> = { ...(c.opts ?? {}) };
    if (typeof serializableOpts.keepSharp === 'function') serializableOpts.keepSharp = 'contains:deadbeef';
    writeFileSync(join(dir, 'opts.json'), JSON.stringify(serializableOpts, null, 2));
    const { body, info } = await transformRequest(bodyBytes, c.opts);
    writeFileSync(join(dir, 'output.json'), body);
    writeFileSync(join(dir, 'info.json'), JSON.stringify(info, null, 2));
    console.log(`transform/${c.name}: compressed=${info.compressed} reason=${info.reason ?? ''} images=${info.imageCount}`);
  }
}

async function main() {
  await genRender();
  await genTransform();
  console.log('done');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
