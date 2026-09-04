//! payout-run 的整合測試：整個程式跑在 solana-program-test 的 BanksClient 上，
//! SPL Token 用的是測試框架內建的那一份，不接外部 RPC。
//!
//! 測試自己養了一份跟鏈下 internal/merkle 鏡像的建樹小工具：真正要防的是兩種實作
//! 對同一份名單算出不同 root，所以第一條測試釘的就是跨語言的 golden root。

use solana_program::hash::hashv;
use solana_program_test::{processor, BanksClientError, ProgramTest, ProgramTestContext};
use solana_sdk::{
    clock::Clock,
    instruction::{AccountMeta, Instruction, InstructionError},
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    system_instruction, system_program,
    transaction::{Transaction, TransactionError},
};

fn program_id() -> Pubkey {
    Pubkey::new_from_array([7u8; 32])
}

fn disc(name: &str) -> [u8; 8] {
    let h = hashv(&[b"global:", name.as_bytes()]);
    let mut out = [0u8; 8];
    out.copy_from_slice(&h.to_bytes()[..8]);
    out
}

// ---- 鏈下那棵樹的鏡像（跟 internal/merkle 同一套規則）----

fn leaf_hash(data: &[u8]) -> [u8; 32] {
    hashv(&[&[0u8], data]).to_bytes()
}

fn payout_leaf(index: u16, merchant: &Pubkey, amount: u64, r: &[u8; 32]) -> [u8; 32] {
    hashv(&[
        &[0u8],
        &index.to_le_bytes(),
        merchant.as_ref(),
        &amount.to_le_bytes(),
        r,
    ])
    .to_bytes()
}

fn node(l: &[u8; 32], r: &[u8; 32]) -> [u8; 32] {
    hashv(&[&[1u8], l, r]).to_bytes()
}

struct Tree {
    levels: Vec<Vec<[u8; 32]>>,
}

fn build(leaves: &[[u8; 32]]) -> Tree {
    let mut width = 8usize;
    while width < leaves.len() {
        width *= 2;
    }
    let mut level = vec![[0u8; 32]; width];
    level[..leaves.len()].copy_from_slice(leaves);
    let mut levels = vec![level];
    while levels.last().unwrap().len() > 1 {
        let prev = levels.last().unwrap();
        let mut next = Vec::with_capacity(prev.len() / 2);
        for i in 0..prev.len() / 2 {
            next.push(node(&prev[2 * i], &prev[2 * i + 1]));
        }
        levels.push(next);
    }
    Tree { levels }
}

impl Tree {
    fn root(&self) -> [u8; 32] {
        self.levels.last().unwrap()[0]
    }
    fn block_proof(&self, block: usize) -> Vec<[u8; 32]> {
        let mut proof = Vec::new();
        let mut idx = block;
        for level in &self.levels[3..self.levels.len() - 1] {
            proof.push(level[idx ^ 1]);
            idx /= 2;
        }
        proof
    }
}

// ---- 測試場景 ----

struct Run {
    payer: Keypair,
    payer_token: Pubkey,
    mint: Pubkey,
    run: Pubkey,
    vault: Pubkey,
    merchants: Vec<Pubkey>,
    amounts: Vec<u64>,
    refs: Vec<[u8; 32]>,
    tree: Tree,
    total: u64,
    leaf_count: u16,
    deadline: i64,
}

async fn latest(context: &mut ProgramTestContext) -> solana_sdk::hash::Hash {
    context.get_new_latest_blockhash().await.unwrap()
}

async fn send(
    context: &mut ProgramTestContext,
    ixs: &[Instruction],
    extra: &[&Keypair],
) -> Result<(), BanksClientError> {
    let blockhash = latest(context).await;
    let mut signers: Vec<&Keypair> = vec![&context.payer];
    signers.extend_from_slice(extra);
    let tx =
        Transaction::new_signed_with_payer(ixs, Some(&context.payer.pubkey()), &signers, blockhash);
    context.banks_client.process_transaction(tx).await
}

/// 開一個 token 帳戶（165 bytes、owner 給定），不經過 ATA 程式：測試只需要「有個地方收錢」。
async fn new_token_account(
    context: &mut ProgramTestContext,
    mint: &Pubkey,
    owner: &Pubkey,
) -> Pubkey {
    let account = Keypair::new();
    let rent = context.banks_client.get_rent().await.unwrap();
    let ixs = [
        system_instruction::create_account(
            &context.payer.pubkey(),
            &account.pubkey(),
            rent.minimum_balance(165),
            165,
            &spl_token::id(),
        ),
        spl_token::instruction::initialize_account3(
            &spl_token::id(),
            &account.pubkey(),
            mint,
            owner,
        )
        .unwrap(),
    ];
    send(context, &ixs, &[&account]).await.unwrap();
    account.pubkey()
}

async fn token_balance(context: &mut ProgramTestContext, account: &Pubkey) -> u64 {
    let acc = context
        .banks_client
        .get_account(*account)
        .await
        .unwrap()
        .expect("token account exists");
    spl_token::state::Account::unpack(&acc.data).unwrap().amount
}

/// setup 造出一輪 n 筆的撥款：mint、payer 與他的 token 帳戶、n 個 merchant 的 token 帳戶、
/// 名單的樹。fund 是 payer 手上有多少錢（total 之外多留一點，才能測「圈存只拿走 total」）。
async fn setup(n: usize, fund: u64) -> (ProgramTestContext, Run) {
    let pt = ProgramTest::new(
        "payout_run",
        program_id(),
        processor!(payout_run::process_instruction),
    );
    let mut context = pt.start_with_context().await;

    let mint = Keypair::new();
    let rent = context.banks_client.get_rent().await.unwrap();
    let authority = context.payer.pubkey();
    let ixs = [
        system_instruction::create_account(
            &context.payer.pubkey(),
            &mint.pubkey(),
            rent.minimum_balance(spl_token::state::Mint::LEN),
            spl_token::state::Mint::LEN as u64,
            &spl_token::id(),
        ),
        spl_token::instruction::initialize_mint2(
            &spl_token::id(),
            &mint.pubkey(),
            &authority,
            None,
            6,
        )
        .unwrap(),
    ];
    send(&mut context, &ixs, &[&mint]).await.unwrap();

    let payer = Keypair::new();
    let fund_payer =
        system_instruction::transfer(&context.payer.pubkey(), &payer.pubkey(), 1_000_000_000);
    send(&mut context, &[fund_payer], &[]).await.unwrap();

    let payer_token = new_token_account(&mut context, &mint.pubkey(), &payer.pubkey()).await;
    let mint_ix = spl_token::instruction::mint_to(
        &spl_token::id(),
        &mint.pubkey(),
        &payer_token,
        &authority,
        &[],
        fund,
    )
    .unwrap();
    send(&mut context, &[mint_ix], &[]).await.unwrap();

    let mut merchants = Vec::with_capacity(n);
    let mut amounts = Vec::with_capacity(n);
    let mut refs = Vec::with_capacity(n);
    for i in 0..n {
        let wallet = Pubkey::new_unique();
        merchants.push(new_token_account(&mut context, &mint.pubkey(), &wallet).await);
        amounts.push(100 + i as u64);
        let mut r = [0u8; 32];
        r[0] = i as u8;
        r[31] = 0xAA;
        refs.push(r);
    }
    let leaves: Vec<[u8; 32]> = (0..n)
        .map(|i| payout_leaf(i as u16, &merchants[i], amounts[i], &refs[i]))
        .collect();
    let tree = build(&leaves);
    let total: u64 = amounts.iter().sum();

    let clock: Clock = context.banks_client.get_sysvar().await.unwrap();
    let deadline = clock.unix_timestamp + 86_400;

    let (run, _) = Pubkey::find_program_address(
        &[b"run", payer.pubkey().as_ref(), &tree.root()],
        &program_id(),
    );
    let (vault, _) = Pubkey::find_program_address(&[b"vault", run.as_ref()], &program_id());

    let run = Run {
        payer,
        payer_token,
        mint: mint.pubkey(),
        run,
        vault,
        merchants,
        amounts,
        refs,
        tree,
        total,
        leaf_count: n as u16,
        deadline,
    };
    (context, run)
}

fn init_ix(r: &Run) -> Instruction {
    let mut data = disc("init_run").to_vec();
    data.extend_from_slice(&r.tree.root());
    data.extend_from_slice(&r.total.to_le_bytes());
    data.extend_from_slice(&r.leaf_count.to_le_bytes());
    data.extend_from_slice(&r.deadline.to_le_bytes());
    Instruction {
        program_id: program_id(),
        accounts: vec![
            AccountMeta::new(r.payer.pubkey(), true),
            AccountMeta::new(r.payer_token, false),
            AccountMeta::new(r.run, false),
            AccountMeta::new(r.vault, false),
            AccountMeta::new_readonly(r.mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
        ],
        data,
    }
}

fn pay_ix(r: &Run, block: usize) -> Instruction {
    let count = (r.merchants.len() - block * 8).min(8);
    let mut data = disc("pay_batch").to_vec();
    data.extend_from_slice(&(block as u16).to_le_bytes());
    data.push(count as u8);
    for i in 0..count {
        let at = block * 8 + i;
        data.extend_from_slice(&r.amounts[at].to_le_bytes());
        data.extend_from_slice(&r.refs[at]);
    }
    for h in r.tree.block_proof(block) {
        data.extend_from_slice(&h);
    }
    let mut accounts = vec![
        AccountMeta::new(r.run, false),
        AccountMeta::new(r.vault, false),
        AccountMeta::new_readonly(spl_token::id(), false),
    ];
    for i in 0..count {
        accounts.push(AccountMeta::new(r.merchants[block * 8 + i], false));
    }
    Instruction {
        program_id: program_id(),
        accounts,
        data,
    }
}

fn claw_ix(r: &Run) -> Instruction {
    Instruction {
        program_id: program_id(),
        accounts: vec![
            AccountMeta::new(r.run, false),
            AccountMeta::new(r.vault, false),
            AccountMeta::new(r.payer_token, false),
            AccountMeta::new(r.payer.pubkey(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: disc("clawback").to_vec(),
    }
}

/// 把時鐘撥到某個時間點。程式只看 Clock 的 unix_timestamp，不看 slot。
async fn warp_to(context: &mut ProgramTestContext, ts: i64) {
    let mut clock: Clock = context.banks_client.get_sysvar().await.unwrap();
    clock.unix_timestamp = ts;
    context.set_sysvar(&clock);
}

fn expect_custom(result: Result<(), BanksClientError>, code: u32) {
    match result {
        Err(BanksClientError::TransactionError(TransactionError::InstructionError(
            _,
            InstructionError::Custom(got),
        ))) => assert_eq!(got, code, "custom error {got}, want {code}"),
        other => panic!("result = {other:?}, want Custom({code})"),
    }
}

// ---- 測試 ----

/// 防的情境：兩種語言各養一棵樹。同一份輸入在 Go 那一側算出的 root 釘在
/// internal/merkle 的 TestBuild_GoldenRootAcrossImplementations，這裡要算出同一個值。
#[test]
fn golden_root_matches_the_go_side() {
    let leaves: Vec<[u8; 32]> = (0..3)
        .map(|i| leaf_hash(format!("leaf-{i}").as_bytes()))
        .collect();
    let tree = build(&leaves);
    assert_eq!(
        hex(&tree.root()),
        "319fbb219345322cf146ff74a9c59b43c5156fd5651c676733df324001e63f66"
    );
}

fn hex(bytes: &[u8; 32]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// 防的情境：圈存拿錯數目。init_run 之後 vault 裡剛好是 total，payer 手上剩 fund - total，
/// 一毛不多一毛不少。
#[tokio::test]
async fn init_run_locks_exactly_the_total() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    assert_eq!(token_balance(&mut context, &r.vault).await, r.total);
    assert_eq!(
        token_balance(&mut context, &r.payer_token).await,
        5_000 - r.total
    );
}

/// 防的情境：一批驗過就到帳。付第 0 個區塊，8 個 merchant 各自收到自己那筆；
/// 同一個區塊再付一次要撞上 AlreadyPaid，這是 root 之外的第二道防線。
#[tokio::test]
async fn pay_batch_pays_a_verified_block_once() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    send(&mut context, &[pay_ix(&r, 0)], &[]).await.unwrap();
    for i in 0..8 {
        assert_eq!(
            token_balance(&mut context, &r.merchants[i]).await,
            r.amounts[i],
            "merchant {i}"
        );
    }
    expect_custom(send(&mut context, &[pay_ix(&r, 0)], &[]).await, 6);
}

/// 防的情境：最後一個區塊沒填滿。20 筆的第 2 個區塊只有 4 筆真葉子，
/// 剩下的位置由全零的 PadLeaf 補，照樣要驗得過、付得出去。
#[tokio::test]
async fn pay_batch_handles_the_padded_final_block() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    send(&mut context, &[pay_ix(&r, 2)], &[]).await.unwrap();
    for i in 16..20 {
        assert_eq!(
            token_balance(&mut context, &r.merchants[i]).await,
            r.amounts[i]
        );
    }
}

/// 防的情境：relayer 想多發一塊錢。金額動一格，葉子就是另一片，root 對不上，整批回滾。
#[tokio::test]
async fn pay_batch_rejects_a_tampered_amount() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    let mut ix = pay_ix(&r, 0);
    // 第 0 項的金額在 disc(8) + block(2) + count(1) 之後，加一個 lamport。
    let tampered = u64::from_le_bytes(ix.data[11..19].try_into().unwrap()) + 1;
    ix.data[11..19].copy_from_slice(&tampered.to_le_bytes());
    expect_custom(send(&mut context, &[ix], &[]).await, 5);
    assert_eq!(token_balance(&mut context, &r.merchants[0]).await, 0);
}

/// 防的情境：relayer 把收款帳戶掉包成自己的。葉子綁的是實際帶進來的帳戶位址，
/// 換一個帳戶就是另一片葉子，一樣走到 InvalidProof。
#[tokio::test]
async fn pay_batch_rejects_a_swapped_receiver() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    let attacker = Pubkey::new_unique();
    let attacker_token = new_token_account(&mut context, &r.mint, &attacker).await;
    let mut ix = pay_ix(&r, 0);
    ix.accounts[3] = AccountMeta::new(attacker_token, false);
    expect_custom(send(&mut context, &[ix], &[]).await, 5);
}

/// 防的情境：一批只付半個區塊。項數跟區塊的真葉子數對不上就整批擋下，
/// 「付了三片剩五片」這種狀態不給存在。
#[tokio::test]
async fn pay_batch_rejects_a_partial_block() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    let mut ix = pay_ix(&r, 0);
    // 砍掉最後一項：count 改 7、資料截掉 40 bytes、帳戶少一個。
    ix.data[10] = 7;
    let cut = ix.data.len() - 40 - r.tree.block_proof(0).len() * 32;
    let proof: Vec<u8> = ix.data[ix.data.len() - r.tree.block_proof(0).len() * 32..].to_vec();
    ix.data.truncate(cut);
    ix.data.extend_from_slice(&proof);
    ix.accounts.truncate(ix.accounts.len() - 1);
    expect_custom(send(&mut context, &[ix], &[]).await, 8);
}

/// 防的情境：過了 clawback 起算點還在付。付款與退回共用同一條時間線，線後只有退回。
#[tokio::test]
async fn pay_batch_stops_at_the_clawback_line() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    warp_to(&mut context, r.deadline).await;
    expect_custom(send(&mut context, &[pay_ix(&r, 0)], &[]).await, 3);
}

/// 防的情境：期限沒到就想把錢拿回去。圈存對 merchant 的意義就是這段期間錢動不了，
/// 「隨時提走」不存在，clawback 只能在線後。
#[tokio::test]
async fn clawback_rejects_before_the_line() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    expect_custom(send(&mut context, &[claw_ix(&r)], &[]).await, 4);
}

/// 防的情境：一輪收尾。付掉第 0 區塊之後 clawback，剩餘退回 payer 的 token 帳戶，
/// vault 與 run 兩個帳戶關掉、rent 的 lamports 回到 payer 手上。
#[tokio::test]
async fn clawback_returns_the_rest_and_the_rent() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    send(&mut context, &[pay_ix(&r, 0)], &[]).await.unwrap();
    let paid: u64 = r.amounts[..8].iter().sum();

    let lamports_before = context
        .banks_client
        .get_account(r.payer.pubkey())
        .await
        .unwrap()
        .unwrap()
        .lamports;
    warp_to(&mut context, r.deadline).await;
    send(&mut context, &[claw_ix(&r)], &[]).await.unwrap();

    assert_eq!(
        token_balance(&mut context, &r.payer_token).await,
        5_000 - r.total + (r.total - paid)
    );
    assert!(context
        .banks_client
        .get_account(r.vault)
        .await
        .unwrap()
        .is_none());
    assert!(context
        .banks_client
        .get_account(r.run)
        .await
        .unwrap()
        .is_none());
    let lamports_after = context
        .banks_client
        .get_account(r.payer.pubkey())
        .await
        .unwrap()
        .unwrap()
        .lamports;
    assert!(lamports_after > lamports_before, "rent did not come back");
}

/// 防的情境：clawback 之後這一輪就結束了，再退一次或再付一批都要被擋。
/// run 帳戶已經關掉，走到的是 WrongRunAccount 而不是任何更客氣的錯。
#[tokio::test]
async fn nothing_works_after_clawback() {
    let (mut context, r) = setup(20, 5_000).await;
    send(&mut context, &[init_ix(&r)], &[&r.payer])
        .await
        .unwrap();
    warp_to(&mut context, r.deadline).await;
    send(&mut context, &[claw_ix(&r)], &[]).await.unwrap();
    expect_custom(send(&mut context, &[pay_ix(&r, 0)], &[]).await, 1);
}
