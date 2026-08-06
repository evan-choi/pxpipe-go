/**
 * Dumps the pxpipe glyph atlases (TS base64 modules) to raw binary files +
 * meta.json for embedding in the Go port via go:embed.
 *
 * Run from the pxpipe submodule root:
 *   pnpm exec tsx ../tools/dump-atlas.ts
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', 'internal', 'atlas', 'data');
mkdirSync(OUT, { recursive: true });

type AtlasMod = {
  cellW: number; cellH: number; ascent: number;
  codepoints: Uint32Array; offsets: Uint32Array;
  wideFlags: Uint8Array; pixels: Uint8Array;
};

function u32leBytes(a: Uint32Array): Uint8Array {
  const out = new Uint8Array(a.length * 4);
  const dv = new DataView(out.buffer);
  for (let i = 0; i < a.length; i++) dv.setUint32(i * 4, a[i]!, true);
  return out;
}

const metas: Record<string, unknown> = {};

function dump(name: string, m: AtlasMod) {
  writeFileSync(join(OUT, `${name}.codepoints.bin.gz`), gzipSync(u32leBytes(m.codepoints)));
  writeFileSync(join(OUT, `${name}.offsets.bin.gz`), gzipSync(u32leBytes(m.offsets)));
  writeFileSync(join(OUT, `${name}.wide.bin.gz`), gzipSync(m.wideFlags));
  writeFileSync(join(OUT, `${name}.pixels.bin.gz`), gzipSync(m.pixels));
  metas[name] = {
    cellW: m.cellW, cellH: m.cellH, ascent: m.ascent,
    numGlyphs: m.codepoints.length,
  };
  console.log(`${name}: ${m.codepoints.length} glyphs, pixels=${m.pixels.length}B`);
}

async function main() {
  const spleen = await import('../pxpipe/src/core/atlas.js');
  dump('spleen-5x8.bit', {
    cellW: spleen.ATLAS_CELL_W, cellH: spleen.ATLAS_CELL_H, ascent: spleen.ATLAS_ASCENT,
    codepoints: spleen.ATLAS_CODEPOINTS, offsets: spleen.ATLAS_OFFSETS,
    wideFlags: spleen.ATLAS_WIDE_FLAGS, pixels: spleen.ATLAS_PIXELS,
  });
  const spleenGray = await import('../pxpipe/src/core/atlas-gray.js');
  dump('spleen-5x8.gray', {
    cellW: spleenGray.ATLAS_GRAY_CELL_W, cellH: spleenGray.ATLAS_GRAY_CELL_H, ascent: spleenGray.ATLAS_GRAY_ASCENT,
    codepoints: spleenGray.ATLAS_GRAY_CODEPOINTS, offsets: spleenGray.ATLAS_GRAY_OFFSETS,
    wideFlags: spleenGray.ATLAS_GRAY_WIDE_FLAGS, pixels: spleenGray.ATLAS_GRAY_PIXELS,
  });
  for (const px of [10, 12, 14]) {
    const bit = await import(`../pxpipe/src/core/atlas-jbmono${px}.js`);
    dump(`jbmono${px}.bit`, {
      cellW: bit.ATLAS_CELL_W, cellH: bit.ATLAS_CELL_H, ascent: bit.ATLAS_ASCENT,
      codepoints: bit.ATLAS_CODEPOINTS, offsets: bit.ATLAS_OFFSETS,
      wideFlags: bit.ATLAS_WIDE_FLAGS, pixels: bit.ATLAS_PIXELS,
    });
    const gray = await import(`../pxpipe/src/core/atlas-gray-jbmono${px}.js`);
    dump(`jbmono${px}.gray`, {
      cellW: gray.ATLAS_GRAY_CELL_W, cellH: gray.ATLAS_GRAY_CELL_H, ascent: gray.ATLAS_GRAY_ASCENT,
      codepoints: gray.ATLAS_GRAY_CODEPOINTS, offsets: gray.ATLAS_GRAY_OFFSETS,
      wideFlags: gray.ATLAS_GRAY_WIDE_FLAGS, pixels: gray.ATLAS_GRAY_PIXELS,
    });
  }
  writeFileSync(join(OUT, 'meta.json'), JSON.stringify(metas, null, 2));
}

main();
