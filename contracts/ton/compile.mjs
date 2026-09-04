// compile.mjs: the vendored FunC sources (contracts/UPSTREAM.md) into build/*.boc.b64 with func-js.
import { compileFunc } from '@ton-community/func-js';
import { Cell } from '@ton/core';
import fs from 'node:fs';

const read = (dir, f) => fs.readFileSync(new URL(`./contracts/${dir}/${f}`, import.meta.url), 'utf8');

async function build(name, dir, targets) {
  const r = await compileFunc({ targets, sources: (p) => read(dir, p) });
  if (r.status === 'error') throw new Error(`${name}: ${r.message}`);
  const code = Cell.fromBoc(Buffer.from(r.codeBoc, 'base64'))[0];
  fs.mkdirSync('build', { recursive: true });
  fs.writeFileSync(`build/${name}.boc.b64`, r.codeBoc);
  console.log(`${name.padEnd(12)} code hash ${code.hash().toString('hex')}`);
}

// token-contract/ft has no #include lines: its build concatenates the files in this order.
await build('ft-wallet', 'ft', ['stdlib.fc', 'params.fc', 'op-codes.fc', 'jetton-utils.fc', 'jetton-wallet.fc']);
await build('ft-minter', 'ft', ['stdlib.fc', 'params.fc', 'op-codes.fc', 'jetton-utils.fc', 'jetton-minter.fc']);
// stablecoin-contract (USDT on TON) resolves its own #includes.
await build('usdt-wallet', 'usdt', ['jetton-wallet.fc']);
await build('usdt-minter', 'usdt', ['jetton-minter.fc']);
