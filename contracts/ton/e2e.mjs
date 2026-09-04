// e2e.mjs: the Go-built W5 request against the real W5 wallet (the code @ton/ton ships) and the real
// jetton contracts (compile.mjs) in @ton/sandbox. Six scenarios; each one's transactions are recorded into
// backend/internal/chain/testdata/tonsandbox/ so TONReader is tested against real contract output.
import { Blockchain } from '@ton/sandbox';
import { Address, beginCell, Cell, toNano, fromNano, external, storeMessage, SendMode, contractAddress } from '@ton/core';
import { WalletContractV5R1 } from '@ton/ton';
import { keyPairFromSeed, sign } from '@ton/crypto';
import { build, raw, cellHash, opOf } from './lib.mjs';
import fs from 'node:fs';
import assert from 'node:assert/strict';

const OP = { transfer: 0xf8a7ea5, internal_transfer: 0x178d4519, notification: 0x7362d09c, excesses: 0xd53276db, payload: 0x3121432c, bounced: 0xffffffff };
const ATTACH = 50_000_000n; // chain.TONAttach
const OUT = new URL('../../backend/internal/chain/testdata/tonsandbox/', import.meta.url).pathname;
const code = (n) => Cell.fromBoc(Buffer.from(fs.readFileSync(`build/${n}.boc.b64`, 'utf8'), 'base64'))[0];

// setup: a funded, deployed W5 wallet at seqno 1, a jetton minter of the given flavour, and jettons minted
// to the wallet's jetton wallet. Keys and the clock are fixed so the recordings are reproducible.
async function setup(flavor, { walletTon = '5', jettons = 1_000_000_000n } = {}) {
  const bc = await Blockchain.create();
  bc.now = 1_800_000_000;
  const admin = await bc.treasury('admin');
  const kp = keyPairFromSeed(Buffer.alloc(32, 7));
  const wallet = bc.openContract(WalletContractV5R1.create({ workchain: 0, publicKey: kp.publicKey }));
  await admin.send({ to: wallet.address, value: toNano(walletTon), bounce: false });
  await wallet.sendTransfer({ seqno: 0, secretKey: kp.secretKey, messages: [], sendMode: SendMode.PAY_GAS_SEPARATELY, timeout: bc.now + 60 });
  assert.equal(await wallet.getSeqno(), 1, 'W5 deployed at seqno 1');

  const walletCode = code(`${flavor}-wallet`), minterCode = code(`${flavor}-minter`);
  const uri = beginCell().storeStringTail('https://example.invalid/jetton.json').endCell();
  const data = flavor === 'ft'
    ? beginCell().storeCoins(0).storeAddress(admin.address).storeRef(beginCell().storeUint(1, 8).storeSlice(uri.beginParse()).endCell()).storeRef(walletCode).endCell()
    : beginCell().storeCoins(0).storeAddress(admin.address).storeAddress(admin.address).storeRef(walletCode).storeRef(uri).endCell();
  const init = { code: minterCode, data };
  const minter = contractAddress(0, init);
  const deployBody = flavor === 'ft' ? beginCell().endCell() : beginCell().storeUint(0xd372158c, 32).storeUint(0, 64).endCell(); // usdt: top_up
  await admin.send({ to: minter, value: toNano('1'), init, body: deployBody, bounce: false });
  assert.equal((await bc.getContract(minter)).accountState?.type, 'active', 'minter deployed');

  const master = beginCell().storeUint(OP.internal_transfer, 32).storeUint(0, 64).storeCoins(jettons)
    .storeAddress(admin.address).storeAddress(admin.address).storeCoins(0).storeBit(0).endCell();
  const mintBody = beginCell().storeUint(flavor === 'ft' ? 21 : 0x642b7d07, 32).storeUint(0, 64)
    .storeAddress(wallet.address).storeCoins(toNano('0.1')).storeRef(master).endCell();
  const mint = await admin.send({ to: minter, value: toNano('0.5'), body: mintBody, bounce: true });
  for (const tx of mint.transactions) assert.ok(!tx.description.aborted, 'mint');
  const ourJW = await walletAddress(bc, minter, wallet.address);
  assert.equal(await balance(bc, ourJW), jettons, 'our jetton wallet holds the minted jettons');
  return { bc, admin, kp, wallet, minter, ourJW, flavor };
}

async function walletAddress(bc, minter, owner) {
  const r = await bc.runGetMethod(minter, 'get_wallet_address', [{ type: 'slice', cell: beginCell().storeAddress(owner).endCell() }]);
  return r.stackReader.readAddress();
}
async function balance(bc, jw) {
  if ((await bc.getContract(jw)).accountState?.type !== 'active') return null;
  return (await bc.runGetMethod(jw, 'get_wallet_data', [])).stackReader.readBigNumber();
}

// merchants are bare addresses with no code, like a merchant who has never touched the chain.
function merchants(n) {
  return Array.from({ length: n }, (_, i) => new Address(0, Buffer.concat([Buffer.alloc(28, 0xa0), Buffer.from([0xde, 0xad, i >> 8, i & 0xff])])));
}

// sendRequest builds with Go, signs the Go hash with the wallet's key, appends the signature the way W5
// expects it (after the signed bits) and sends the external message.
async function sendRequest(env, items) {
  const seqno = await env.wallet.getSeqno();
  const spec = { Wallet: raw(env.wallet.address), JettonWallet: raw(env.ourJW), Seqno: seqno, ValidUntil: env.bc.now + 300,
    Payouts: items.map((it, i) => ({ IntentID: `pi_${String(i + 1).padStart(4, '0')}`, Merchant: raw(it.to), Amount: it.amount.toString() })) };
  const r = build(spec);
  assert.equal(cellHash(r.signing), r.SigningHash, '@ton/core re-reads the Go BoC to the same hash');
  const sig = sign(Buffer.from(r.SigningHash, 'hex'), env.kp.secretKey);
  const body = beginCell().storeSlice(r.signing.beginParse()).storeBuffer(sig).endCell();
  const msg = external({ to: env.wallet.address, body });
  const extHash = cellHash(beginCell().store(storeMessage(msg)).endCell());
  const res = await env.bc.sendMessage(msg);
  assert.equal(await env.wallet.getSeqno(), seqno + 1, 'the wallet spent the seqno');
  return { r, res, extHash, seqno };
}

const addrOf = (tx) => new Address(0, Buffer.from(tx.address.toString(16).padStart(64, '0'), 'hex'));
const msgCell = (m) => beginCell().store(storeMessage(m)).endCell();
function describe(tx) {
  const cp = tx.description.computePhase, ap = tx.description.actionPhase;
  return { exit: cp.type === 'vm' ? cp.exitCode : `skipped:${cp.reason}`, gas: cp.type === 'vm' ? Number(cp.gasUsed) : 0,
    actions: ap ? `${ap.success ? 'ok' : 'FAIL(' + ap.resultCode + ')'} ${ap.totalActions}` : '-', aborted: tx.description.aborted, out: tx.outMessagesCount };
}

// tree serialises a cell for the Go side: the bits as a 0/1 string, the refs recursively, and @ton/core's hash.
const tree = (c) => ({ Bits: [...Array(c.bits.length)].map((_, i) => (c.bits.at(i) ? '1' : '0')).join(''), Refs: c.refs.map(tree), Hash: cellHash(c) });

// record writes the transactions of one or more sends the way TONNode would answer for them:
// masterchain seqno = 100 + the hop's depth under its external message (wallet 101, our jetton wallet 102, ...).
function record(file, sends, roles) {
  const txs = [];
  for (const { res } of sends) {
    const depth = new Map();
    for (const tx of res.transactions) {
      const inm = tx.inMessage;
      const d = inm.info.type === 'external-in' ? 1 : depth.get(cellHash(msgCell(inm))) ?? 1;
      const conv = (m) => {
        const i = m.info;
        return { Hash: cellHash(msgCell(m)), Src: i.type === 'internal' ? raw(i.src) : '', Dst: raw(i.dest), Value: i.type === 'internal' ? i.value.coins.toString() : '0',
          Bounce: i.type === 'internal' ? i.bounce : false, Bounced: i.type === 'internal' ? i.bounced : false, Body: m.body.bits.length || m.body.refs.length ? tree(m.body) : null };
      };
      const out = [];
      for (const [, m] of tx.outMessages) { out.push(conv(m)); depth.set(out[out.length - 1].Hash, d + 1); }
      const cp = tx.description.computePhase;
      txs.push({ Account: raw(addrOf(tx)), Role: roles(addrOf(tx)), LT: tx.lt.toString(), Hash: tx.hash().toString('hex'), In: conv(inm), Out: out,
        Aborted: tx.description.aborted, ExitCode: cp.type === 'vm' ? cp.exitCode : 0, Masterchain: 100 + d });
    }
  }
  fs.mkdirSync(OUT, { recursive: true });
  fs.writeFileSync(`${OUT}${file}.json`, JSON.stringify({ Txs: txs, Externals: sends.map((s) => s.extHash), Payouts: sends.map((s) => s.r.Payouts) }, null, 1));
}

function roleFn(env, ms, jws) {
  return (a) => {
    if (a.equals(env.wallet.address)) return 'wallet';
    if (a.equals(env.ourJW)) return 'our jetton wallet';
    if (a.equals(env.minter)) return 'minter';
    if (a.equals(env.admin.address)) return 'admin';
    const m = ms.findIndex((x) => x.equals(a)), j = jws.findIndex((x) => x.equals(a));
    return m >= 0 ? `merchant ${m}` : j >= 0 ? `merchant ${j}'s jetton wallet` : `stranger ${a.toRawString().slice(0, 12)}`;
  };
}

function printTxs(res, roles, limit = 40) {
  for (const tx of res.transactions.slice(0, limit)) {
    const d = describe(tx);
    const op = tx.inMessage.body.bits.length >= 32 ? tx.inMessage.body.beginParse().loadUint(32).toString(16) : '(empty)';
    const kind = tx.inMessage.info.type === 'external-in' ? 'external' : tx.inMessage.info.bounced ? 'bounced' : 'internal';
    console.log(`    ${roles(addrOf(tx)).padEnd(28)} ${kind.padEnd(8)} op ${op.padEnd(8)} exit ${String(d.exit).padEnd(14)} gas ${String(d.gas).padStart(6)} actions ${d.actions.padEnd(8)} out ${d.out}${d.aborted ? '  ABORTED' : ''}`);
  }
  if (res.transactions.length > limit) console.log(`    ... ${res.transactions.length - limit} more`);
}

// checkDelivered verifies each payout landed: the balance, the notification carrying our payload, excesses back home.
async function checkDelivered(env, ms, jws, sent, items) {
  const { res, r } = sent;
  let spent = 0n;
  for (let i = 0; i < items.length; i++) {
    const ref = Buffer.from(r.Payouts[i].Ref, 'hex');
    assert.equal(await balance(env.bc, jws[i]), items[i].amount, `merchant ${i} jetton balance`);
    const jwTx = res.transactions.find((tx) => addrOf(tx).equals(jws[i]) && !tx.description.aborted);
    assert.ok(jwTx, `merchant ${i}'s jetton wallet ran internal_transfer`);
    let note, excess;
    for (const [, m] of jwTx.outMessages) {
      const op = opOf(m.body);
      if (op === OP.notification) note = m; else if (op === OP.excesses) excess = m;
    }
    assert.ok(note && note.info.dest.equals(ms[i]) && !note.info.bounce, `merchant ${i} got a non-bounceable transfer_notification`);
    const s = note.body.beginParse(); s.loadUint(32);
    assert.equal(s.loadUintBig(64), ref.readBigUInt64BE(0), 'query_id is the first 8 bytes of the ref');
    assert.equal(s.loadCoins(), items[i].amount, 'notification amount');
    assert.ok(s.loadAddress().equals(env.wallet.address), 'notification sender is our wallet');
    assert.equal(s.loadBit(), true, 'forward_payload rides in a ref');
    const p = s.loadRef().beginParse();
    assert.equal(p.loadUint(32), OP.payload, 'payload op');
    assert.ok(p.loadBuffer(32).equals(ref), 'payload carries the ref untouched');
    assert.equal(p.remainingBits, 0);
    assert.ok(excess && excess.info.dest.equals(env.wallet.address), `excesses for payout ${i} went back to the wallet`);
    spent += ATTACH - excess.info.value.coins;
  }
  return spent;
}

async function scenarioDelivered(flavor, n) {
  console.log(`\n== ${flavor}: ${n} payouts, all delivered ==`);
  const env = await setup(flavor, { walletTon: n > 10 ? '40' : '5', jettons: BigInt(n) * 100_000_000n + 1n });
  const ms = merchants(n);
  const items = ms.map((to) => ({ to, amount: 100_000_000n }));
  const before = (await env.bc.getContract(env.wallet.address)).balance;
  const sent = await sendRequest(env, items);
  const walletTx = sent.res.transactions[0];
  const d = describe(walletTx);
  console.log(`  request: ${sent.r.Size} bytes, ${sent.r.Cells} cells, depth ${sent.r.Depth}; wallet tx exit ${d.exit}, gas ${d.gas}, actions ${d.actions}, out ${d.out}`);
  assert.equal(d.exit, 0); assert.equal(d.out, n); assert.ok(!d.aborted);
  // the wallet must emit the payouts in list order: the OutList wraps the first payout innermost.
  const order = [...walletTx.outMessages.values()].map((m) => { const q = m.body.beginParse(); q.loadUint(32); const id = q.loadUintBig(64); return sent.r.Payouts.findIndex((p) => Buffer.from(p.Ref, 'hex').readBigUInt64BE(0) === id) + 1; });
  assert.deepEqual(order, items.map((_, i) => i + 1), 'payouts leave the wallet in list order');
  console.log(`  wallet emitted the payouts in list order (${order.length > 6 ? order.slice(0, 3).join(', ') + ' ... ' + order.slice(-2).join(', ') : order.join(', ')})`);
  const jws = await Promise.all(ms.map((m) => walletAddress(env.bc, env.minter, m)));
  const roles = roleFn(env, ms, jws);
  printTxs(sent.res, roles, n <= 3 ? 40 : 6);
  // the merchant itself is a bare address: its transfer_notification tx is skipped for lack of gas (1 nanoton), which is what the non-bounceable hop is for.
  for (const tx of sent.res.transactions) {
    if (tx.inMessage.info.type === 'internal' && opOf(tx.inMessage.body) === OP.notification) { assert.equal(describe(tx).exit, 'skipped:no-gas'); continue; }
    assert.ok(!tx.description.aborted, `aborted tx at ${roles(addrOf(tx))}: exit ${describe(tx).exit}`);
  }
  const spent = await checkDelivered(env, ms, jws, sent, items);
  assert.equal(await balance(env.bc, env.ourJW), 1n, 'our jetton wallet paid everything out');
  const after = (await env.bc.getContract(env.wallet.address)).balance;
  console.log(`  every payout landed; the chain kept ${fromNano(spent / BigInt(n))} TON of the 0.05 attached per payout; wallet ${fromNano(before)} -> ${fromNano(after)} (${fromNano((before - after) / BigInt(n))} per payout)`);
  record(`${flavor}-delivered-${n}`, [sent], roles);
}

async function scenarioRejected(flavor) {
  console.log(`\n== ${flavor}: our jetton wallet refuses (balance 50, payout 100) ==`);
  const env = await setup(flavor, { jettons: 50_000_000n });
  const ms = merchants(1);
  const sent = await sendRequest(env, [{ to: ms[0], amount: 100_000_000n }]);
  const jws = await Promise.all(ms.map((m) => walletAddress(env.bc, env.minter, m)));
  const roles = roleFn(env, ms, jws);
  printTxs(sent.res, roles);
  const ours = sent.res.transactions.find((tx) => addrOf(tx).equals(env.ourJW));
  assert.ok(ours.description.aborted, 'our jetton wallet aborted');
  assert.equal(await balance(env.bc, env.ourJW), 50_000_000n, 'jettons never left');
  const bounced = [...ours.outMessages.values()].find((m) => m.info.bounced);
  assert.ok(bounced && bounced.info.dest.equals(env.wallet.address), 'a bounced message went back to the wallet');
  const s = bounced.body.beginParse();
  assert.equal(s.loadUint(32), OP.bounced); assert.equal(s.loadUint(32), OP.transfer);
  const landed = sent.res.transactions.find((tx) => cellHash(msgCell(tx.inMessage)) === cellHash(msgCell(bounced)));
  assert.ok(landed && !landed.description.aborted, 'the wallet took the bounce');
  assert.equal(await balance(env.bc, jws[0]), null, 'merchant jetton wallet was never deployed');
  console.log(`  exit ${describe(ours).exit}; bounced body is 0xffffffff + the transfer's first ${bounced.body.bits.length - 32} bits; jettons still ${await balance(env.bc, env.ourJW)}`);
  record(`${flavor}-rejected`, [sent], roles);
}

async function scenarioBounced() {
  console.log(`\n== usdt: the merchant's jetton wallet is receive-locked, internal_transfer bounces ==`);
  const env = await setup('usdt', { jettons: 300_000_000n });
  const ms = merchants(1);
  const first = await sendRequest(env, [{ to: ms[0], amount: 100_000_000n }]);
  const jws = await Promise.all(ms.map((m) => walletAddress(env.bc, env.minter, m)));
  const roles = roleFn(env, ms, jws);
  assert.equal(await balance(env.bc, jws[0]), 100_000_000n, 'the first payout landed and deployed the wallet');
  // the admin locks the merchant's wallet for incoming transfers: call_to(set_status 2)
  const setStatus = beginCell().storeUint(0xeed236d3, 32).storeUint(0, 64).storeUint(2, 4).endCell();
  const lock = await env.admin.send({ to: env.minter, value: toNano('0.2'), bounce: true,
    body: beginCell().storeUint(0x235caf52, 32).storeUint(0, 64).storeAddress(ms[0]).storeCoins(toNano('0.05')).storeRef(setStatus).endCell() });
  for (const tx of lock.transactions) assert.ok(!tx.description.aborted, 'lock');
  assert.equal((await env.bc.runGetMethod(jws[0], 'get_status', [])).stackReader.readNumber(), 2, 'receive-locked');
  const second = await sendRequest(env, [{ to: ms[0], amount: 100_000_000n }]);
  printTxs(second.res, roles);
  const theirs = second.res.transactions.find((tx) => addrOf(tx).equals(jws[0]));
  assert.ok(theirs.description.aborted, 'merchant wallet aborted');
  assert.equal(describe(theirs).exit, 45, 'contract_locked');
  const bounced = [...theirs.outMessages.values()].find((m) => m.info.bounced);
  assert.ok(bounced && bounced.info.dest.equals(env.ourJW), 'internal_transfer bounced back to our jetton wallet');
  const s = bounced.body.beginParse();
  assert.equal(s.loadUint(32), OP.bounced); assert.equal(s.loadUint(32), OP.internal_transfer); s.loadUintBig(64);
  assert.equal(s.loadCoins(), 100_000_000n, 'the bounced body still carries the amount for on_bounce');
  const landed = second.res.transactions.find((tx) => cellHash(msgCell(tx.inMessage)) === cellHash(msgCell(bounced)));
  assert.ok(landed && !landed.description.aborted, 'on_bounce ran on our jetton wallet');
  assert.equal(await balance(env.bc, env.ourJW), 200_000_000n, '300 minus the 100 that landed; the bounced 100 is back');
  assert.equal(await balance(env.bc, jws[0]), 100_000_000n, 'the merchant still has only the first payout');
  console.log(`  exit 45 at the merchant; on_bounce restored our balance to ${await balance(env.bc, env.ourJW)}, the merchant still has ${await balance(env.bc, jws[0])}`);
  record('usdt-bounced', [first, second], roles);
}

await scenarioDelivered('ft', 3);
await scenarioDelivered('usdt', 3);
await scenarioDelivered('ft', 255);
await scenarioRejected('ft');
await scenarioRejected('usdt');
await scenarioBounced();
console.log(`\ne2e: all six scenarios passed; recordings in ${OUT}`);
