// lib.mjs: the bridge to the Go side and a few helpers shared by golden.mjs and e2e.mjs.
import { execFileSync } from 'node:child_process';
import { Cell } from '@ton/core';

const BACKEND = new URL('../../backend', import.meta.url).pathname;

// build hands a request spec to backend/cmd/tondump, which builds it with chain.TON.TransferRequest
// and answers with the signing cell (BoC), its hash, and every payout's ref, body and message.
export function build(spec) {
  const out = execFileSync('go', ['-C', BACKEND, 'run', './cmd/tondump'], { input: JSON.stringify(spec), stdio: ['pipe', 'pipe', 'inherit'] });
  const r = JSON.parse(out.toString());
  r.signing = Cell.fromBoc(Buffer.from(r.SigningBoc, 'hex'))[0];
  return r;
}

export const raw = (a) => a.toRawString();
export const hex = (b) => Buffer.from(b).toString('hex');
export const cellHash = (c) => c.hash().toString('hex');
export const opOf = (body) => (body.bits.length >= 32 ? body.beginParse().loadUint(32) : -1);
