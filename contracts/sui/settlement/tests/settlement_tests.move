/// 結算模組的測試。每一條防一個真實情境，名字讀起來就是那個情境。
///
/// 三個角色跟其他三條鏈一樣：payer 付錢、merchant 收錢、operator 拿著 OperatorCap；
/// relayer 在這條鏈上只出現在一個地方，就是替 payer 出 gas 的 sponsor。
#[test_only]
module settlement::settlement_tests;

use settlement::settlement::{Self, Book, Hold, OperatorCap, Paid};
use sui::clock;
use sui::coin::{Self, Coin};
use sui::event;
use sui::test_scenario as ts;

/// 測試用的穩定幣。mint_for_testing 對任何型別都能鑄，不需要 TreasuryCap。
public struct USDC has drop {}

const PAYER: address = @0xA11CE;
const MERCHANT: address = @0xB0B;
const OPERATOR: address = @0x0E0;
const RELAYER: address = @0x5E1A;
const FEE_RECIPIENT: address = @0xFEE;
const STRANGER: address = @0x51;

const REF_1: vector<u8> = x"1111111111111111111111111111111111111111111111111111111111111111";
const REF_2: vector<u8> = x"2222222222222222222222222222222222222222222222222222222222222222";

/// payer 開一本 Book 再拿回來用：open_book 把它轉給 payer，下一筆交易才取得到。
fun open_book(s: &mut ts::Scenario, payer: address): Book {
    s.next_tx(payer);
    settlement::open_book(s.ctx());
    s.next_tx(payer);
    s.take_from_sender<Book>()
}

/// 防的情境：最基本的一筆付款。merchant 名下要多一顆剛好等於金額的 coin，Book 要記住 ref，
/// 而且 sponsor 那一欄是 none：這筆是 payer 自己出 gas。
#[test]
fun test_pay_hands_the_coin_to_the_merchant_and_records_the_ref() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    assert!(book.payer() == PAYER);

    let coin = coin::mint_for_testing<USDC>(100_000_000, s.ctx());
    settlement::pay(&mut book, coin, MERCHANT, REF_1, s.ctx());
    assert!(book.is_paid(REF_1));
    assert!(!book.is_paid(REF_2));
    let paid = event::events_by_type<Paid<USDC>>();
    assert!(paid.length() == 1);
    s.return_to_sender(book);

    s.next_tx(MERCHANT);
    let got = s.take_from_sender<Coin<USDC>>();
    assert!(got.value() == 100_000_000);
    coin::burn_for_testing(got);
    s.end();
}

/// 防的情境：整個系列在防的那件事。同一把 ref 再付一次，錢不能動第二次。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefAlreadyPaid)]
fun test_pay_refuses_a_ref_that_was_already_paid() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    abort
}

/// 防的情境：ref 是 32 bytes 的承諾，長度不對就是拿錯了東西當 ref。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefLength)]
fun test_pay_refuses_a_ref_that_is_not_32_bytes() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, x"1111", s.ctx());
    abort
}

/// 防的情境：零值的 ref 代表「還沒算」，不是一把 ref。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefZero)]
fun test_pay_refuses_a_zero_ref() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let zero = x"0000000000000000000000000000000000000000000000000000000000000000";
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, zero, s.ctx());
    abort
}

/// 防的情境：一顆面額為零的 coin 也是合法的 coin，但一筆零元的付款不是合法的付款。
#[test, expected_failure(abort_code = ::settlement::settlement::EAmountZero)]
fun test_pay_refuses_a_zero_amount() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    settlement::pay(&mut book, coin::zero<USDC>(s.ctx()), MERCHANT, REF_1, s.ctx());
    abort
}

/// 防的情境：付給零地址等於把 coin 燒掉。
#[test, expected_failure(abort_code = ::settlement::settlement::EMerchantZero)]
fun test_pay_refuses_the_zero_merchant() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), @0x0, REF_1, s.ctx());
    abort
}

/// 防的情境：這道防線的範圍。Book 是每個 payer 一本，兩個 payer 各自拿同一把 ref 付款，
/// 兩筆都會過：第二筆不是「我們的錢動了第二次」，是另一個人的錢帶著我們的 ref，那是對帳引擎的事。
/// 這條測試守住的是「Book 不是全網共用的」這個決定，改成 shared object 的話它會壞掉。
#[test]
fun test_two_payers_keep_two_books() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    s.return_to_sender(book);

    let mut other = open_book(&mut s, STRANGER);
    settlement::pay(&mut other, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    assert!(other.is_paid(REF_1));
    s.return_to_sender(other);
    s.end();
}

/// 防的情境：relayer 替 payer 出 gas。付款的 Paid event 要記下出錢的是誰，對帳才對得回
/// 「這筆是哪個 relayer 送的」；payer 那一欄照舊是 Book 的主人，不是出 gas 的人。
#[test]
fun test_pay_records_the_sponsor_that_paid_for_gas() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let sponsored = s.ctx_builder().set_sponsor(RELAYER);
    s.next_with_context(sponsored);
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    let paid = event::events_by_type<Paid<USDC>>();
    assert!(paid.length() == 1);
    assert!(settlement::paid_sponsor(&paid[0]) == option::some(RELAYER));
    assert!(settlement::paid_payer(&paid[0]) == PAYER);
    s.return_to_sender(book);
    s.end();
}

/// 防的情境：託管的完整一輪。錢先進 Hold 這個 object（payer 與 merchant 名下都沒有它），
/// operator release 之後 merchant 拿到 amount 減 fee、fee_recipient 拿到 fee，Hold 消失。
#[test]
fun test_hold_then_release_splits_the_fee() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let clock = clock::create_for_testing(s.ctx());
    settlement::hold(
        &mut book,
        coin::mint_for_testing<USDC>(100_000_000, s.ctx()),
        MERCHANT,
        1_000_000,
        FEE_RECIPIENT,
        REF_1,
        86_400_000,
        &clock,
        s.ctx(),
    );
    assert!(book.is_paid(REF_1));
    s.return_to_sender(book);

    s.next_tx(OPERATOR);
    let hold = s.take_shared<Hold<USDC>>();
    assert!(hold.held_amount() == 100_000_000);
    let cap = settlement::operator_cap_for_testing(s.ctx());
    settlement::release(&cap, hold, s.ctx());
    transfer::public_transfer(cap, OPERATOR);

    s.next_tx(MERCHANT);
    let got = s.take_from_sender<Coin<USDC>>();
    assert!(got.value() == 99_000_000);
    coin::burn_for_testing(got);
    s.next_tx(FEE_RECIPIENT);
    let fee = s.take_from_sender<Coin<USDC>>();
    assert!(fee.value() == 1_000_000);
    coin::burn_for_testing(fee);
    assert!(!ts::has_most_recent_shared<Hold<USDC>>());
    clock.destroy_for_testing();
    s.end();
}

/// 防的情境：operator 提前退款，整筆回到 payer，手續費一毛不收。
#[test]
fun test_refund_by_the_operator_returns_everything() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let clock = clock::create_for_testing(s.ctx());
    settlement::hold(
        &mut book,
        coin::mint_for_testing<USDC>(100_000_000, s.ctx()),
        MERCHANT,
        1_000_000,
        FEE_RECIPIENT,
        REF_1,
        86_400_000,
        &clock,
        s.ctx(),
    );
    s.return_to_sender(book);

    s.next_tx(OPERATOR);
    let hold = s.take_shared<Hold<USDC>>();
    let cap = settlement::operator_cap_for_testing(s.ctx());
    settlement::refund(&cap, hold, s.ctx());
    transfer::public_transfer(cap, OPERATOR);

    s.next_tx(PAYER);
    let back = s.take_from_sender<Coin<USDC>>();
    assert!(back.value() == 100_000_000);
    coin::burn_for_testing(back);
    clock.destroy_for_testing();
    s.end();
}

/// 防的情境：payer 的保底。過了 refund_after_ms，payer 自己就能退，不需要 OperatorCap。
#[test]
fun test_refund_expired_needs_no_operator_after_the_window() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let mut clock = clock::create_for_testing(s.ctx());
    settlement::hold(
        &mut book,
        coin::mint_for_testing<USDC>(100_000_000, s.ctx()),
        MERCHANT,
        0,
        @0x0,
        REF_1,
        86_400_000,
        &clock,
        s.ctx(),
    );
    s.return_to_sender(book);

    s.next_tx(PAYER);
    clock.set_for_testing(86_400_000);
    let hold = s.take_shared<Hold<USDC>>();
    settlement::refund_expired(hold, &clock, s.ctx());

    s.next_tx(PAYER);
    let back = s.take_from_sender<Coin<USDC>>();
    assert!(back.value() == 100_000_000);
    coin::burn_for_testing(back);
    clock.destroy_for_testing();
    s.end();
}

/// 防的情境：期限還沒到，payer 自己退不了。這段時間錢動不了，就是託管對 merchant 的意義。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefundWindowNotOpen)]
fun test_refund_expired_is_refused_before_the_window() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let mut clock = clock::create_for_testing(s.ctx());
    settlement::hold(
        &mut book,
        coin::mint_for_testing<USDC>(100_000_000, s.ctx()),
        MERCHANT,
        0,
        @0x0,
        REF_1,
        86_400_000,
        &clock,
        s.ctx(),
    );
    s.return_to_sender(book);

    s.next_tx(PAYER);
    clock.set_for_testing(86_399_999);
    let hold = s.take_shared<Hold<USDC>>();
    settlement::refund_expired(hold, &clock, s.ctx());
    abort
}

/// 防的情境：shared object 誰都塞得進交易裡。期限過了，路人拿著 Hold 來退，錢雖然會回到 payer
/// 手上，但「誰能收掉這筆託管」不該是路人決定的。
#[test, expected_failure(abort_code = ::settlement::settlement::ENotThePayer)]
fun test_refund_expired_is_refused_for_a_stranger() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let mut clock = clock::create_for_testing(s.ctx());
    settlement::hold(
        &mut book,
        coin::mint_for_testing<USDC>(100_000_000, s.ctx()),
        MERCHANT,
        0,
        @0x0,
        REF_1,
        86_400_000,
        &clock,
        s.ctx(),
    );
    s.return_to_sender(book);

    s.next_tx(STRANGER);
    clock.set_for_testing(86_400_000);
    let hold = s.take_shared<Hold<USDC>>();
    settlement::refund_expired(hold, &clock, s.ctx());
    abort
}

/// 防的情境：手續費吃掉整筆金額。
#[test, expected_failure(abort_code = ::settlement::settlement::EFeeNotLessThanAmount)]
fun test_hold_refuses_a_fee_that_is_not_less_than_the_amount() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let clock = clock::create_for_testing(s.ctx());
    settlement::hold(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, 100, FEE_RECIPIENT, REF_1, 1, &clock, s.ctx());
    abort
}

/// 防的情境：收手續費卻沒說要付給誰。
#[test, expected_failure(abort_code = ::settlement::settlement::EFeeRecipientZero)]
fun test_hold_refuses_a_fee_without_a_recipient() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let clock = clock::create_for_testing(s.ctx());
    settlement::hold(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, 1, @0x0, REF_1, 1, &clock, s.ctx());
    abort
}

/// 防的情境：期限寫成過去的時間，等於 payer 當場就能退，託管形同虛設。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefundAfterNotInFuture)]
fun test_hold_refuses_a_refund_window_that_is_already_open() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let mut clock = clock::create_for_testing(s.ctx());
    clock.set_for_testing(1_000);
    settlement::hold(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, 0, @0x0, REF_1, 1_000, &clock, s.ctx());
    abort
}

/// 防的情境：託管跟當場結清共用同一本 Book。一把 ref 託管過就不能再付，反過來也一樣。
#[test, expected_failure(abort_code = ::settlement::settlement::ERefAlreadyPaid)]
fun test_hold_and_pay_share_the_same_book() {
    let mut s = ts::begin(PAYER);
    let mut book = open_book(&mut s, PAYER);
    let clock = clock::create_for_testing(s.ctx());
    settlement::hold(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, 0, @0x0, REF_1, 1, &clock, s.ctx());
    settlement::pay(&mut book, coin::mint_for_testing<USDC>(100, s.ctx()), MERCHANT, REF_1, s.ctx());
    abort
}

/// 防的情境：發布模組的那筆交易要把 OperatorCap 交到發布者手上，而且只有一顆。
#[test]
fun test_init_hands_the_operator_cap_to_the_publisher() {
    let mut s = ts::begin(OPERATOR);
    settlement::init_for_testing(s.ctx());
    let effects = s.next_tx(OPERATOR);
    assert!(effects.created().length() == 1);
    let cap = s.take_from_sender<OperatorCap>();
    assert!(!s.has_most_recent_for_sender<OperatorCap>());
    s.return_to_sender(cap);
    s.end();
}
