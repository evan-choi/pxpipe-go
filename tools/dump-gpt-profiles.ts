import { resolveGptProfile, isMisresolvedModelId } from '../../pxpipe/src/core/gpt-model-profiles.js';
const ids = ['gpt-5.6-sol','gpt-5.6-sol-20260101','gpt-5.6-terra','gpt-5.4','gpt-5.5-codex','gpt-5','gpt-5-chat-latest','gpt-5-mini','gpt-5.2-nano','gpt-4.1-mini','gpt-4.1-nano','o4-mini','o1','o3-mini','gpt-4o','gpt-4.1','grok-4.5','grok-3','gemini-3.6-flash','google/gemini-3.6-flash','gemini-3.6-pro','claude-fable-5','claude-3-5-sonnet','anthropic/claude-opus-5','moonshotai/kimi-k3','openrouter/gpt-5.4','unknown-model'];
const out: Record<string, unknown> = {}; const guards: Record<string, boolean> = {};
for (const id of ids) { out[id] = resolveGptProfile(id); guards[id] = isMisresolvedModelId(id); }
console.log(JSON.stringify({ profiles: out, misresolved: guards }, null, 1));
