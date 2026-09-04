// golden.mjs: rebuilds the three requests pinned in backend/internal/chain/tonmsg_test.go with @ton/ton,
// strips the trailing 512-bit signature, and compares hash and bytes with what the Go builder emits.
// The three constants below are the same ones the Go test pins; a mismatch on either side fails here.
import { beginCell, Address, internal, SendMode, storeMessageRelaxed } from '@ton/core';
import { createWalletTransferV5R1 } from '@ton/ton/dist/wallets/signing/createWalletTransfer.js';
import { storeWalletIdV5R1 } from '@ton/ton/dist/wallets/v5r1/WalletV5R1WalletId.js';
import { keyPairFromSeed } from '@ton/crypto';
import { build, cellHash, hex } from './lib.mjs';

const PINNED = {
  1: '11477c77b380ddd89d666ec1301f8804aba66bda80f3a340973c6a5a87cd4d24',
  12: 'ea0b3151dc289944aaa01872a7ce4713d204e00203eb5ad3323e868746b6a175',
  255: 'd1dbacd41cb2ca51e5488d6593eff5b4670f69f16b4ae61694324dcc4dd63ed1',
};
const PINNED_BODY = 'f5ba54a3f91743ab0260b746f8f22dda28222eaf471becdd138bb262aa71204c';
const PINNED_MESSAGE = '73a7772522003c31c50846ee396f847c4411e83444cd30ce14c82b16899adaf4';

const PAYLOAD_OP = 0x3121432c;
const wallet = Address.parse('0:1111111111111111111111111111111111111111111111111111111111111111');
const jettonWallet = Address.parse('0:2222222222222222222222222222222222222222222222222222222222222222');
const kp = keyPairFromSeed(Buffer.alloc(32, 1));

// tonTransferBody is TEP-74's transfer written with @ton/core, independently of the Go builder.
function tonTransferBody(p) {
  const ref = Buffer.from(p.Ref, 'hex');
  return beginCell()
    .storeUint(0xf8a7ea5, 32)
    .storeUint(ref.readBigUInt64BE(0), 64)
    .storeCoins(BigInt(p.Amount))
    .storeAddress(Address.parse(p.Merchant))
    .storeAddress(wallet)
    .storeMaybeRef(null)
    .storeCoins(1n)
    .storeMaybeRef(beginCell().storeUint(PAYLOAD_OP, 32).storeBuffer(ref).endCell())
    .endCell();
}

let ok = true;
for (const n of [1, 12, 255]) {
  const go = build({ Wallet: wallet.toRawString(), JettonWallet: jettonWallet.toRawString(), Seqno: 41, ValidUntil: 1_800_000_300, Golden: n });
  const actions = go.Payouts.map((p) => ({
    type: 'sendMsg',
    mode: SendMode.PAY_GAS_SEPARATELY, // @ton/ton adds +2 (IGNORE_ERRORS) itself for external auth
    outMsg: internal({ to: jettonWallet, value: 50_000_000n, bounce: true, body: tonTransferBody(p) }),
  }));
  const signed = createWalletTransferV5R1({
    seqno: 41, timeout: 1_800_000_300, actions, secretKey: kp.secretKey, authType: 'external',
    walletId: storeWalletIdV5R1({ networkGlobalId: -239, context: { workchain: 0, walletVersion: 'v5r1', subwalletNumber: 0 } }),
  });
  // Strip the 512-bit signature from the tail; keep the refs.
  const s = signed.beginParse();
  const b = beginCell().storeBits(s.loadBits(s.remainingBits - 512));
  while (s.remainingRefs > 0) b.storeRef(s.loadRef());
  const unsigned = b.endCell();
  const tsHash = cellHash(unsigned);
  const sameBytes = hex(unsigned.toBoc({ idx: false, crc32: true })) === go.SigningBoc;
  const same = tsHash === go.SigningHash && sameBytes && go.SigningHash === PINNED[n];
  ok &&= same;
  console.log(`n=${String(n).padStart(3)}  @ton/ton ${tsHash}  go ${go.SigningHash}  pinned ${PINNED[n].slice(0, 8)}…  boc ${sameBytes ? 'identical' : 'DIFFERENT'} (${go.Size} bytes, ${go.Cells} cells, depth ${go.Depth})  ${same ? 'OK' : 'MISMATCH'}`);
  if (n === 1) {
    const body = tonTransferBody(go.Payouts[0]);
    const msg = beginCell().store(storeMessageRelaxed(actions[0].outMsg)).endCell();
    const bodyOK = cellHash(body) === PINNED_BODY, msgOK = cellHash(msg) === PINNED_MESSAGE;
    ok &&= bodyOK && msgOK;
    console.log(`       body ${cellHash(body)} ${bodyOK ? 'OK' : 'MISMATCH'}   message ${cellHash(msg)} ${msgOK ? 'OK' : 'MISMATCH'}`);
  }
}
console.log(ok ? 'golden: the Go builder, @ton/ton and the pinned constants agree' : 'golden: MISMATCH');
process.exit(ok ? 0 : 1);
