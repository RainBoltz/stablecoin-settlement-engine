/// 結算模組的 Sui 版本：同一組付款規則（一把 ref 只付一次、託管先鎖後放、手續費在結清那一刻才收），
/// 換到一條「錢是 object、誰能動它寫在 object 上」的鏈。
///
/// EVM 那份 `Settlement` 合約是一個地方：錢經過它、狀態住在它裡面、`msg.sender` 決定誰能叫哪個入口。
/// 這裡沒有那個地方。`Coin<T>` 是 payer 名下的 object，只有 payer 簽的交易帶得動它，模組拿不到
/// allowance、也沒有 `transferFrom` 可以叫（address-owned 的 object「Other addresses cannot access owned
/// objects in any way」，https://docs.sui.io/concepts/object-ownership/address-owned）。所以 EVM 那四個入口
/// 裡靠 allowance 的兩個（`settle`、`settleWithPermit`）在這裡不存在，留下來的只有 payer 自己把 coin 交進來
/// 的 `pay`，以及先鎖後放的 `hold`；一批付款不是模組的事，是一個 PTB 的事。
///
/// 狀態也一樣要有一個家。EVM 上 `paid` 是合約自己 storage 裡的一個 mapping，全網共用、不用選；這裡它必須
/// 是一個 object，而這個 object 歸誰決定了它的成本：
///
///   - 做成一個全網共用的 shared object：每一筆付款都得經過共識排序，而且所有 payer 擠同一個 object
///     （「Contention on a single shared object can reduce transaction throughput」，
///     https://docs.sui.io/concepts/object-ownership）。它擋得住陌生人拿我們的 ref 付款，但也讓陌生人
///     能搶先取用我們的 ref。
///   - 做成每個 payer 一本、payer 自己持有的 `Book`：走 fastpath、不排共識，能取用這本 Book 的只有
///     payer 自己簽的交易。它擋的是「同一個 payer 把同一把 ref 付兩次」，也就是這個系統真正要防的
///     那件事：relayer 重試、job 重送、payer 端重放。陌生人帶著我們的 ref 付錢不會被擋，那筆錢從來
///     不是我們的錢動了第二次，對帳引擎照舊把它列成 unexpected。
///
/// 這裡選後者。缺點寫在 `Book` 上：同一個 payer 兩筆同時在路上的交易會撞同一個 object 版本，這是
/// 這條鏈的 equivocation 規則管的事，撥款這條線上一輪名單本來就收進同一筆交易。
///
/// 託管反過來必須是 shared object：release 由 operator 發起、期限後的 refund 由 payer 發起，兩個
/// 地址都要碰得到同一個 `Hold`。而 shared object 誰都能塞進交易裡，權限只能在 Move 裡自己檢查
/// （「secure it in Move with explicit authorization checks (such as a capability argument, ...)」，
/// https://docs.sui.io/concepts/object-ownership）：operator 的身分是一個 `OperatorCap` object，誰持有它誰
/// 就能 release，取代 EVM 上的 `isRelayer[msg.sender]`。
///
/// 本模組為本系列從零設計，只取公開設計裡需要的那部分。
module settlement::settlement;

use sui::balance::Balance;
use sui::clock::Clock;
use sui::coin::{Self, Coin};
use sui::event;
use sui::table::{Self, Table};

/// ref 是 paymentref 算出來的 32 bytes，跟其他三條鏈上的同一把。
const REF_LEN: u64 = 32;
const ZERO_REF: vector<u8> = x"0000000000000000000000000000000000000000000000000000000000000000";

#[error]
const ERefLength: vector<u8> = b"the ref is not 32 bytes";
#[error]
const ERefZero: vector<u8> = b"the ref is zero";
#[error]
const EMerchantZero: vector<u8> = b"the merchant is the zero address";
#[error]
const EAmountZero: vector<u8> = b"the amount is zero";
#[error]
const ERefAlreadyPaid: vector<u8> = b"the ref was already paid";
#[error]
const EFeeNotLessThanAmount: vector<u8> = b"the fee is not less than the amount";
#[error]
const EFeeRecipientZero: vector<u8> = b"the fee recipient is the zero address";
#[error]
const ERefundAfterNotInFuture: vector<u8> = b"refund_after is not in the future";
#[error]
const ENotThePayer: vector<u8> = b"only the payer can refund an expired hold";
#[error]
const ERefundWindowNotOpen: vector<u8> = b"the refund window is not open";

/// operator 的身分：持有這個 object 的地址才能 release 或提前 refund。
/// 它在模組發布時鑄一顆給發布者；帶 `store` 是為了讓 operator 換人時能把它整顆轉走，
/// 而不是重新發布模組。
public struct OperatorCap has key, store { id: UID }

/// 一個 payer 付過的 ref。EVM 上這是合約裡的 `paid` mapping，這裡每個 payer 一本、payer 自己持有。
///
/// 只有 `key`、沒有 `store`：模組外面沒有任何辦法把它轉給別人，所以一本 Book 開在哪個地址就永遠
/// 在哪個地址，付過的 ref 不會跟著一次轉帳消失在另一個帳戶裡。`paid` 是一張 Table，每一把
/// ref 是一個 dynamic field：付一筆就多一個永遠不會刪掉的 object，storage 費用也就永遠不退，
/// 這是這道防線在這條鏈上的價錢。
public struct Book has key {
    id: UID,
    payer: address,
    paid: Table<vector<u8>, bool>,
}

/// 一筆託管：錢真的住在這個 object 的 `funds` 裡，而不是住在模組裡。
///
/// shared object，理由見模組註解。條件（fee、fee_recipient、refund_after_ms）在 hold 那一刻寫死，
/// release 只是照著拆；payer 過了 refund_after_ms 不必等任何人點頭就能把整筆拿回去。
public struct Hold<phantom T> has key {
    id: UID,
    ref: vector<u8>,
    payer: address,
    merchant: address,
    funds: Balance<T>,
    fee: u64,
    fee_recipient: address,
    refund_after_ms: u64,
}

/// 一筆當場結清。`sponsor` 是替這筆交易出 gas 的地址：有 relayer 代付就是 relayer，payer 自己付
/// 就是 none。EVM 的 `Paid` 記的是 `msg.sender`，這裡沒有那個人，出錢的人才是對帳要記的角色。
public struct Paid<phantom T> has copy, drop {
    ref: vector<u8>,
    payer: address,
    merchant: address,
    amount: u64,
    sponsor: Option<address>,
}

/// 一筆託管開始：錢進了 `hold` 這個 object。
public struct Held<phantom T> has copy, drop {
    hold: ID,
    ref: vector<u8>,
    payer: address,
    merchant: address,
    amount: u64,
    fee: u64,
    refund_after_ms: u64,
}

/// 一筆託管結清：amount 減 fee 給 merchant，fee 給 fee_recipient。
public struct Released<phantom T> has copy, drop {
    hold: ID,
    ref: vector<u8>,
    merchant: address,
    amount: u64,
    fee: u64,
    fee_recipient: address,
}

/// 一筆託管退回：整筆回到 payer，手續費一毛不收。
public struct Refunded<phantom T> has copy, drop {
    hold: ID,
    ref: vector<u8>,
    payer: address,
    amount: u64,
}

/// 發布模組的那筆交易鑄一顆 OperatorCap 給發布者。之後要換 operator，把它轉走就好。
fun init(ctx: &mut TxContext) {
    transfer::transfer(OperatorCap { id: object::new(ctx) }, ctx.sender());
}

/// payer 替自己開一本 Book。誰開的就歸誰，而且從此離不開那個地址（見 Book）。
public fun open_book(ctx: &mut TxContext) {
    transfer::transfer(
        Book { id: object::new(ctx), payer: ctx.sender(), paid: table::new(ctx) },
        ctx.sender(),
    );
}

/// 當場結清：`coin` 整顆付給 merchant，金額就是這顆 coin 的面額。
///
/// coin 用值傳進來而不是 `&mut Coin` 加一個金額：要付多少，由呼叫端在 PTB 裡先 SplitCoins
/// 切好，模組只認「交進來的那顆」，不替 payer 決定要從哪顆 coin 扣。Move 沒有重入這回事，
/// 先記 ref 再搬錢的順序在這裡只是讓四條鏈的規則讀起來一樣。
public fun pay<T>(book: &mut Book, coin: Coin<T>, merchant: address, ref: vector<u8>, ctx: &mut TxContext) {
    let amount = coin.value();
    reserve(book, merchant, amount, ref);
    event::emit(Paid<T> { ref, payer: book.payer, merchant, amount, sponsor: ctx.sponsor() });
    transfer::public_transfer(coin, merchant);
}

/// 先鎖後放：`coin` 進 `Hold` 這個 object 裡，等 operator 來 release，或等過了 refund_after_ms
/// 由 payer 自己 refund。
///
/// fee 與 fee_recipient 在這裡就講定、寫進 object，release 只能照著拆；refund_after_ms 是 payer 的
/// 保底，用鏈上的 Clock 比，不用交易自己帶的時間。錢進來就是整筆進來：這條鏈上一顆 coin 的
/// 面額就是它的面額，沒有轉帳稅那種東西要檢查實收。
public fun hold<T>(
    book: &mut Book,
    coin: Coin<T>,
    merchant: address,
    fee: u64,
    fee_recipient: address,
    ref: vector<u8>,
    refund_after_ms: u64,
    clock: &Clock,
    ctx: &mut TxContext,
) {
    let amount = coin.value();
    reserve(book, merchant, amount, ref);
    assert!(fee < amount, EFeeNotLessThanAmount);
    assert!(fee == 0 || fee_recipient != @0x0, EFeeRecipientZero);
    assert!(refund_after_ms > clock.timestamp_ms(), ERefundAfterNotInFuture);

    let hold = Hold<T> {
        id: object::new(ctx),
        ref,
        payer: book.payer,
        merchant,
        funds: coin.into_balance(),
        fee,
        fee_recipient,
        refund_after_ms,
    };
    event::emit(Held<T> {
        hold: object::id(&hold),
        ref,
        payer: book.payer,
        merchant,
        amount,
        fee,
        refund_after_ms,
    });
    transfer::share_object(hold);
}

/// 結清一筆託管：amount 減 fee 給 merchant，fee 給 fee_recipient，`Hold` 這個 object 跟著消失。
///
/// 第一個參數就是權限：拿不出 OperatorCap 的交易根本組不出這個呼叫，不用在函式裡檢查誰在叫。
/// 手續費在這一刻才真的收到，退回去的託管一毛都不收（見 refund）。
public fun release<T>(_: &OperatorCap, hold: Hold<T>, ctx: &mut TxContext) {
    let Hold { id, ref, payer: _, merchant, mut funds, fee, fee_recipient, refund_after_ms: _ } = hold;
    let hold_id = id.to_inner();
    id.delete();
    let amount = funds.value();
    if (fee > 0) {
        transfer::public_transfer(coin::take(&mut funds, fee, ctx), fee_recipient);
    };
    event::emit(Released<T> { hold: hold_id, ref, merchant, amount: amount - fee, fee, fee_recipient });
    transfer::public_transfer(funds.into_coin(ctx), merchant);
}

/// operator 隨時可以把一筆託管整筆退給 payer。
public fun refund<T>(_: &OperatorCap, hold: Hold<T>, ctx: &mut TxContext) {
    refund_to_payer(hold, ctx);
}

/// 過了 refund_after_ms，payer 自己就能把整筆拿回去，不需要 operator 點頭。
///
/// 這是錢住在 object 裡那段時間的信任邊界：payer 不必相信 operator 總有一天會來處理，
/// 時間一到自己就拿得回來。跟 EVM 上的 refund 一樣只讓 payer 本人退，不讓路人代退：
/// 期限到了不等於 operator 失去 release 的資格，誰先送到鏈上誰算數。
public fun refund_expired<T>(hold: Hold<T>, clock: &Clock, ctx: &mut TxContext) {
    assert!(ctx.sender() == hold.payer, ENotThePayer);
    assert!(clock.timestamp_ms() >= hold.refund_after_ms, ERefundWindowNotOpen);
    refund_to_payer(hold, ctx);
}

/// 這本 Book 是哪個 payer 的。
public fun payer(book: &Book): address { book.payer }

/// 這把 ref 在這本 Book 上付過了沒。
public fun is_paid(book: &Book, ref: vector<u8>): bool { book.paid.contains(ref) }

/// 一筆託管鎖著多少錢。
public fun held_amount<T>(hold: &Hold<T>): u64 { hold.funds.value() }

/// 一筆 Paid 記的 payer：Book 的主人，不是出 gas 的人。
public fun paid_payer<T>(e: &Paid<T>): address { e.payer }

/// 一筆 Paid 記的 sponsor：替這筆交易出 gas 的地址，payer 自己出就是 none。
public fun paid_sponsor<T>(e: &Paid<T>): Option<address> { e.sponsor }

/// 每一個搬錢的入口都先經過這裡：檢查參數、確認這把 ref 在這本 Book 上沒付過，然後記下來。
///
/// 全額退回的託管，ref 仍然算付過：修正靠新的 intent、新的 ref，這條規矩四條鏈都一樣。
fun reserve(book: &mut Book, merchant: address, amount: u64, ref: vector<u8>) {
    assert!(ref.length() == REF_LEN, ERefLength);
    assert!(ref != ZERO_REF, ERefZero);
    assert!(merchant != @0x0, EMerchantZero);
    assert!(amount > 0, EAmountZero);
    assert!(!book.paid.contains(ref), ERefAlreadyPaid);
    book.paid.add(ref, true);
}

/// 退回的共用路徑：`Hold` 消失、整筆 funds 變回一顆 coin 給 payer。
fun refund_to_payer<T>(hold: Hold<T>, ctx: &mut TxContext) {
    let Hold { id, ref, payer, merchant: _, funds, fee: _, fee_recipient: _, refund_after_ms: _ } = hold;
    let hold_id = id.to_inner();
    id.delete();
    event::emit(Refunded<T> { hold: hold_id, ref, payer, amount: funds.value() });
    transfer::public_transfer(funds.into_coin(ctx), payer);
}

#[test_only]
/// 測試用：跳過發布那筆交易，直接鑄一顆 OperatorCap。
public fun operator_cap_for_testing(ctx: &mut TxContext): OperatorCap {
    OperatorCap { id: object::new(ctx) }
}

#[test_only]
/// 測試用：跑一次 init，確認發布者拿到 OperatorCap。
public fun init_for_testing(ctx: &mut TxContext) { init(ctx) }
