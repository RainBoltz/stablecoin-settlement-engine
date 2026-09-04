//! payout-run：把「一輪撥款」收成鏈上的一個帳戶，payer 只簽一筆交易。
//!
//! 那一筆交易做三件事：把整輪的總額圈存進 vault、把整份名單的 merkle root 寫上鏈、
//! 把 clawback 的期限與去向寫死。之後的每一筆付款交易都由 relayer 簽名與代付手續費，
//! 程式只認 root：一批帶著「對齊區塊走回 root」的證明進來，重算出同一個 root 才付錢，
//! 所以 relayer 拿不走名單以外的一毛錢，最壞就是擺爛不付，而不付的部分過了期限
//! 由 clawback 原路退回 payer。
//!
//! 樹的形狀跟鏈下 internal/merkle 那一份互為鏡像：葉子是 sha256(0x00 || 內容)、
//! 內部節點是 sha256(0x01 || 左 || 右)、名單墊到 2 的冪次、批對齊在 8 片葉子的區塊上。
//! 葉子的內容編碼定義在這裡：index（u16 LE）、merchant 的 token 帳戶（32）、
//! 金額（u64 LE）、ref（32）。merchant 的地址取自交易實際帶進來的帳戶，
//! 所以帳戶換一個，葉子就是另一片，證明當場過不了。
//!
//! 正式專案請直接用 Anchor 與經過審計的 distributor（Saber、Jito 的 merkle-distributor
//! 都是公開的）；這裡照它們的行為手寫最小子集，是為了把機制拆開。指令的 discriminator
//! 沿用 Anchor 的慣例（sha256("global:<名字>") 的前 8 個 bytes），讓鏈下組訊息的那一側
//! 不用管這個程式是不是 Anchor 寫的。
//!
//! 本程式為本系列從零設計，只取公開設計裡需要的那部分。

use solana_program::{
    account_info::{next_account_info, AccountInfo},
    clock::Clock,
    entrypoint::ProgramResult,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    msg,
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey::Pubkey,
    rent::Rent,
    sysvar::Sysvar,
};
// system_instruction 在 solana-program 2.x 標了棄用、要人改接 solana-system-interface；
// 這個程式的依賴刻意只有 solana-program 一份，多接一個 crate 換一個警告不划算，之後真的移除再跟。
#[allow(deprecated)]
use solana_program::system_instruction;

#[cfg(not(feature = "no-entrypoint"))]
solana_program::entrypoint!(process_instruction);

/// 一批最多幾片葉子，也是對齊區塊的大小。跟鏈下 bulk.Defaults() 的 Align 是同一個 8：
/// 16 片的批連 proof 都還沒算就塞不進 1,232 bytes 的交易了。
pub const BLOCK: usize = 8;

/// run 帳戶的固定表頭長度，bitmap 接在後面、一片葉子一個 bit。
const HEADER: usize = 126;

const OFF_VERSION: usize = 0;
const OFF_BUMP: usize = 1;
const OFF_VAULT_BUMP: usize = 2;
const OFF_CLAWED: usize = 3;
const OFF_PAYER: usize = 4;
const OFF_MINT: usize = 36;
const OFF_ROOT: usize = 68;
const OFF_TOTAL: usize = 100;
const OFF_PAID: usize = 108;
const OFF_DEADLINE: usize = 116;
const OFF_LEAVES: usize = 124;

const VERSION: u8 = 1;

/// 程式的錯誤。編號進 ProgramError::Custom，測試釘的就是這些數字。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PayoutError {
    /// run 帳戶不是這個程式的、或版本對不上。
    WrongRunAccount = 1,
    /// 這一輪已經 clawback 過了，什麼都不能再做。
    AlreadyClawedBack = 2,
    /// 過了 clawback 的起算點還想付款。付款與 clawback 用同一條線切開，
    /// 兩邊才不會在期限邊上賽跑。
    RunExpired = 3,
    /// 還沒到 clawback 的起算點就想把錢拿回去。
    TooEarlyToClawback = 4,
    /// 重算出來的 root 跟 payer 簽的那一個對不上：名單被改過、順序被換過、
    /// 或帳戶被掉包，全部走到這裡。
    InvalidProof = 5,
    /// 這個區塊已經付過了。一個 bit 一片葉子，翻起來就不會再翻回去。
    AlreadyPaid = 6,
    /// 區塊編號超出名單。
    BlockOutOfRange = 7,
    /// 一批的項數跟區塊裡實際的葉子數對不上：批一定整個區塊一起付，不能只付一半。
    WrongBatchShape = 8,
    /// clawback 的錢想去 payer 以外的地方。去向在 init_run 就寫死了。
    WrongReceiver = 9,
    /// clawback 的期限沒有在未來。一開場就過期的 run 一筆都付不出去。
    DeadlineNotInFuture = 10,
}

impl From<PayoutError> for ProgramError {
    fn from(e: PayoutError) -> Self {
        ProgramError::Custom(e as u32)
    }
}

/// Anchor 慣例的 instruction discriminator：sha256("global:<名字>") 的前 8 個 bytes。
/// https://www.anchor-lang.com/docs/basics/idl
fn discriminator(name: &str) -> [u8; 8] {
    let h = hashv(&[b"global:", name.as_bytes()]);
    let mut out = [0u8; 8];
    out.copy_from_slice(&h.to_bytes()[..8]);
    out
}

pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if data.len() < 8 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let (disc, rest) = data.split_at(8);
    if disc == discriminator("init_run") {
        init_run(program_id, accounts, rest)
    } else if disc == discriminator("pay_batch") {
        pay_batch(program_id, accounts, rest)
    } else if disc == discriminator("clawback") {
        clawback(program_id, accounts)
    } else {
        Err(ProgramError::InvalidInstructionData)
    }
}

/// init_run：payer 唯一要簽的那一筆。圈存總額、寫入 root 與 clawback 條款。
///
/// 資料：root（32）、total（u64 LE）、leaf_count（u16 LE）、clawback_start_ts（i64 LE）。
/// 帳戶：payer（簽名、出 rent）、payer 的 token 帳戶、run PDA、vault PDA、mint、
/// SPL Token 程式、System 程式。
///
/// vault 不是 associated token account：它是本程式自己的 PDA，開成一個 owner 是
/// run PDA 的 token 帳戶。少接一個程式，形狀也少一種。
fn init_run(program_id: &Pubkey, accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    if data.len() != 50 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let root: [u8; 32] = data[..32].try_into().unwrap();
    let total = u64::from_le_bytes(data[32..40].try_into().unwrap());
    let leaf_count = u16::from_le_bytes(data[40..42].try_into().unwrap());
    let deadline = i64::from_le_bytes(data[42..50].try_into().unwrap());
    if total == 0 || leaf_count == 0 {
        return Err(ProgramError::InvalidInstructionData);
    }

    let it = &mut accounts.iter();
    let payer = next_account_info(it)?;
    let payer_token = next_account_info(it)?;
    let run = next_account_info(it)?;
    let vault = next_account_info(it)?;
    let mint = next_account_info(it)?;
    let token_program = next_account_info(it)?;
    // System 程式一定要在帳戶清單裡（create_account 的 CPI 用它），但這裡不需要讀它。
    let _system_program = next_account_info(it)?;

    if !payer.is_signer {
        return Err(ProgramError::MissingRequiredSignature);
    }
    if Clock::get()?.unix_timestamp >= deadline {
        return Err(PayoutError::DeadlineNotInFuture.into());
    }

    // run 與 vault 都是從 payer 與 root 推導出來的位址：同一個 payer、同一份名單，
    // 只會有同一個 run。位址對不上就是呼叫端把帳戶排錯了。
    let (run_key, bump) =
        Pubkey::find_program_address(&[b"run", payer.key.as_ref(), &root], program_id);
    if run_key != *run.key {
        return Err(PayoutError::WrongRunAccount.into());
    }
    let (vault_key, vault_bump) =
        Pubkey::find_program_address(&[b"vault", run_key.as_ref()], program_id);
    if vault_key != *vault.key {
        return Err(PayoutError::WrongRunAccount.into());
    }

    let space = HEADER + (leaf_count as usize).div_ceil(8);
    let rent = Rent::get()?;
    invoke_signed(
        &system_instruction::create_account(
            payer.key,
            run.key,
            rent.minimum_balance(space),
            space as u64,
            program_id,
        ),
        &[payer.clone(), run.clone()],
        &[&[b"run", payer.key.as_ref(), &root, &[bump]]],
    )?;
    invoke_signed(
        &system_instruction::create_account(
            payer.key,
            vault.key,
            rent.minimum_balance(165),
            165,
            token_program.key,
        ),
        &[payer.clone(), vault.clone()],
        &[&[b"vault", run_key.as_ref(), &[vault_bump]]],
    )?;
    // InitializeAccount3（tag 18）：把剛開好的帳戶初始化成 mint 的 token 帳戶，
    // owner 設成 run PDA，之後只有這個程式簽得動裡面的錢。
    let mut init_data = Vec::with_capacity(33);
    init_data.push(18);
    init_data.extend_from_slice(run.key.as_ref());
    invoke(
        &Instruction {
            program_id: *token_program.key,
            accounts: vec![
                AccountMeta::new(*vault.key, false),
                AccountMeta::new_readonly(*mint.key, false),
            ],
            data: init_data,
        },
        &[vault.clone(), mint.clone()],
    )?;
    // 圈存：總額從 payer 的 token 帳戶一次付進 vault。這筆的簽名者是 payer 本人，
    // 也是整輪撥款裡 payer 唯一一次簽名。
    invoke(
        &transfer(
            token_program.key,
            payer_token.key,
            vault.key,
            payer.key,
            total,
        ),
        &[payer_token.clone(), vault.clone(), payer.clone()],
    )?;

    let mut state = run.try_borrow_mut_data()?;
    state[OFF_VERSION] = VERSION;
    state[OFF_BUMP] = bump;
    state[OFF_VAULT_BUMP] = vault_bump;
    state[OFF_CLAWED] = 0;
    state[OFF_PAYER..OFF_PAYER + 32].copy_from_slice(payer.key.as_ref());
    state[OFF_MINT..OFF_MINT + 32].copy_from_slice(mint.key.as_ref());
    state[OFF_ROOT..OFF_ROOT + 32].copy_from_slice(&root);
    state[OFF_TOTAL..OFF_TOTAL + 8].copy_from_slice(&total.to_le_bytes());
    state[OFF_PAID..OFF_PAID + 8].copy_from_slice(&0u64.to_le_bytes());
    state[OFF_DEADLINE..OFF_DEADLINE + 8].copy_from_slice(&deadline.to_le_bytes());
    state[OFF_LEAVES..OFF_LEAVES + 2].copy_from_slice(&leaf_count.to_le_bytes());
    Ok(())
}

/// pay_batch：relayer 帶一個對齊區塊回來，程式驗過 root 才付錢。
///
/// 資料：block（u16 LE）、count（u8）、count 組（amount u64 LE、ref 32）、
/// 接著整份證明（(depth-3) x 32）。
/// 帳戶：run、vault、SPL Token 程式，然後照名單順序放 count 個 merchant 的 token 帳戶。
///
/// 一批一定把區塊裡真實的葉子全部付完，不能只付一半：交易本來就是原子的，
/// 「付了三片剩五片」這種狀態沒有存在的理由，少帶一片就整批擋下。
fn pay_batch(program_id: &Pubkey, accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    if data.len() < 3 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let block = u16::from_le_bytes(data[..2].try_into().unwrap()) as usize;
    let count = data[2] as usize;
    if count == 0 || count > BLOCK || data.len() < 3 + count * 40 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let proof_bytes = &data[3 + count * 40..];
    if proof_bytes.len() % 32 != 0 {
        return Err(ProgramError::InvalidInstructionData);
    }

    let it = &mut accounts.iter();
    let run = next_account_info(it)?;
    let vault = next_account_info(it)?;
    let token_program = next_account_info(it)?;

    let (root, bump, payer, leaf_count, deadline) = {
        let state = run.try_borrow_data()?;
        check_run(program_id, run, &state)?;
        if state[OFF_CLAWED] != 0 {
            return Err(PayoutError::AlreadyClawedBack.into());
        }
        let mut root = [0u8; 32];
        root.copy_from_slice(&state[OFF_ROOT..OFF_ROOT + 32]);
        let mut payer = [0u8; 32];
        payer.copy_from_slice(&state[OFF_PAYER..OFF_PAYER + 32]);
        (
            root,
            state[OFF_BUMP],
            payer,
            u16::from_le_bytes(state[OFF_LEAVES..OFF_LEAVES + 2].try_into().unwrap()) as usize,
            i64::from_le_bytes(state[OFF_DEADLINE..OFF_DEADLINE + 8].try_into().unwrap()),
        )
    };
    // 過了 clawback 的起算點就不再付款。跟 clawback 共用同一條線，兩邊不會賽跑：
    // 線前只有付款、線後只有退回。
    if Clock::get()?.unix_timestamp >= deadline {
        return Err(PayoutError::RunExpired.into());
    }

    if block * BLOCK >= leaf_count {
        return Err(PayoutError::BlockOutOfRange.into());
    }
    let expect = (leaf_count - block * BLOCK).min(BLOCK);
    if count != expect {
        return Err(PayoutError::WrongBatchShape.into());
    }
    let depth = depth_for(leaf_count);
    if proof_bytes.len() != (depth - 3) * 32 {
        return Err(PayoutError::InvalidProof.into());
    }

    // 重算葉子。merchant 的地址取自實際帶進來的帳戶：帳戶掉包，葉子就對不上 root。
    let merchants: Vec<&AccountInfo> = it.collect();
    if merchants.len() != count {
        return Err(PayoutError::WrongBatchShape.into());
    }
    let mut level = [[0u8; 32]; BLOCK];
    let mut amounts = [0u64; BLOCK];
    for i in 0..BLOCK {
        if i < count {
            let at = 3 + i * 40;
            let amount = u64::from_le_bytes(data[at..at + 8].try_into().unwrap());
            let index = (block * BLOCK + i) as u16;
            level[i] = hashv(&[
                &[0u8],
                &index.to_le_bytes(),
                merchants[i].key.as_ref(),
                &amount.to_le_bytes(),
                &data[at + 8..at + 40],
            ])
            .to_bytes();
            amounts[i] = amount;
        } else {
            level[i] = [0u8; 32]; // 墊出來的葉子：全零，跟鏈下的 PadLeaf 同一個值。
        }
    }
    // 區塊自己的 3 層先收成區塊的根，再拿證明一層一層走回 root。
    let mut width = BLOCK;
    while width > 1 {
        for i in 0..width / 2 {
            level[i] = hashv(&[&[1u8], &level[2 * i], &level[2 * i + 1]]).to_bytes();
        }
        width /= 2;
    }
    let mut acc = level[0];
    let mut idx = block;
    for sibling in proof_bytes.chunks(32) {
        acc = if idx % 2 == 0 {
            hashv(&[&[1u8], &acc, sibling]).to_bytes()
        } else {
            hashv(&[&[1u8], sibling, &acc]).to_bytes()
        };
        idx /= 2;
    }
    if acc != root {
        return Err(PayoutError::InvalidProof.into());
    }

    // 證明過了才動狀態：先把整個區塊的 bit 翻起來，再逐筆付出去。
    // 交易是原子的，中途任何一筆轉帳失敗，翻過的 bit 會跟著整筆回滾。
    let mut paid_total = 0u64;
    {
        let mut state = run.try_borrow_mut_data()?;
        for i in 0..count {
            let leaf = block * BLOCK + i;
            let byte = HEADER + leaf / 8;
            let mask = 1u8 << (leaf % 8);
            if state[byte] & mask != 0 {
                return Err(PayoutError::AlreadyPaid.into());
            }
            state[byte] |= mask;
        }
    }
    let seeds: &[&[u8]] = &[b"run", &payer, &root, &[bump]];
    for i in 0..count {
        invoke_signed(
            &transfer(
                token_program.key,
                vault.key,
                merchants[i].key,
                run.key,
                amounts[i],
            ),
            &[vault.clone(), merchants[i].clone(), run.clone()],
            &[seeds],
        )?;
        paid_total = paid_total
            .checked_add(amounts[i])
            .ok_or(ProgramError::InvalidInstructionData)?;
        msg!("paid leaf {}", block * BLOCK + i);
    }
    let mut state = run.try_borrow_mut_data()?;
    let paid = u64::from_le_bytes(state[OFF_PAID..OFF_PAID + 8].try_into().unwrap());
    let paid = paid
        .checked_add(paid_total)
        .ok_or(ProgramError::InvalidInstructionData)?;
    state[OFF_PAID..OFF_PAID + 8].copy_from_slice(&paid.to_le_bytes());
    Ok(())
}

/// clawback：過了起算點，把 vault 裡剩下的錢原路退回 payer，並把兩個帳戶關掉收回 rent。
///
/// 帳戶：run、vault、payer 的 token 帳戶、payer 本人（收回 rent 的 lamports）、SPL Token 程式。
/// 收錢的 token 帳戶的 owner 必須是 init_run 記下的那個 payer：去向在開場就寫死，
/// 這裡只是照著執行。
fn clawback(program_id: &Pubkey, accounts: &[AccountInfo]) -> ProgramResult {
    let it = &mut accounts.iter();
    let run = next_account_info(it)?;
    let vault = next_account_info(it)?;
    let payer_token = next_account_info(it)?;
    let payer = next_account_info(it)?;
    let token_program = next_account_info(it)?;

    let (root, bump, payer_key, deadline) = {
        let state = run.try_borrow_data()?;
        check_run(program_id, run, &state)?;
        if state[OFF_CLAWED] != 0 {
            return Err(PayoutError::AlreadyClawedBack.into());
        }
        let mut root = [0u8; 32];
        root.copy_from_slice(&state[OFF_ROOT..OFF_ROOT + 32]);
        let mut pk = [0u8; 32];
        pk.copy_from_slice(&state[OFF_PAYER..OFF_PAYER + 32]);
        (
            root,
            state[OFF_BUMP],
            pk,
            i64::from_le_bytes(state[OFF_DEADLINE..OFF_DEADLINE + 8].try_into().unwrap()),
        )
    };
    if Clock::get()?.unix_timestamp < deadline {
        return Err(PayoutError::TooEarlyToClawback.into());
    }
    if payer.key.as_ref() != payer_key {
        return Err(PayoutError::WrongReceiver.into());
    }
    // token 帳戶的 owner 欄位在資料的第 32..64 bytes（SPL Token 的 Account 版面）。
    // https://github.com/solana-program/token/blob/main/interface/src/state.rs
    {
        let dest = payer_token.try_borrow_data()?;
        if dest.len() < 64 || dest[32..64] != payer_key {
            return Err(PayoutError::WrongReceiver.into());
        }
    }

    let remaining = {
        let v = vault.try_borrow_data()?;
        u64::from_le_bytes(v[64..72].try_into().unwrap())
    };
    let seeds: &[&[u8]] = &[b"run", &payer_key, &root, &[bump]];
    if remaining > 0 {
        invoke_signed(
            &transfer(
                token_program.key,
                vault.key,
                payer_token.key,
                run.key,
                remaining,
            ),
            &[vault.clone(), payer_token.clone(), run.clone()],
            &[seeds],
        )?;
    }
    // CloseAccount（tag 9）：把 vault 關掉，rent 的 lamports 退給 payer。
    invoke_signed(
        &Instruction {
            program_id: *token_program.key,
            accounts: vec![
                AccountMeta::new(*vault.key, false),
                AccountMeta::new(*payer.key, false),
                AccountMeta::new_readonly(*run.key, true),
            ],
            data: vec![9],
        },
        &[vault.clone(), payer.clone(), run.clone()],
        &[seeds],
    )?;
    // run 自己也關掉：資料清空、lamports 全數退回 payer。clawed_back 不用留在鏈上，
    // 帳戶消失本身就是這一輪結束的證明，而留著它就是永遠退不回去的 rent。
    {
        let mut state = run.try_borrow_mut_data()?;
        state.fill(0);
    }
    let lamports = run.lamports();
    **run.try_borrow_mut_lamports()? = 0;
    **payer.try_borrow_mut_lamports()? = payer
        .lamports()
        .checked_add(lamports)
        .ok_or(ProgramError::InvalidInstructionData)?;
    Ok(())
}

/// check_run：run 帳戶要是本程式的、版本要對。
fn check_run(program_id: &Pubkey, run: &AccountInfo, state: &[u8]) -> ProgramResult {
    if run.owner != program_id || state.len() < HEADER || state[OFF_VERSION] != VERSION {
        return Err(PayoutError::WrongRunAccount.into());
    }
    Ok(())
}

/// depth_for：名單墊到 2 的冪次（至少一個區塊）之後樹有幾層。跟鏈下 bulk 算的是同一個數。
fn depth_for(leaf_count: usize) -> usize {
    let mut width = BLOCK;
    let mut depth = 3;
    while width < leaf_count {
        width *= 2;
        depth += 1;
    }
    depth
}

/// transfer：SPL Token 的 Transfer（tag 3）。手寫指令 bytes 而不是接 spl-token 的 crate，
/// 理由跟鏈下只用標準函式庫一樣：這個程式的依賴只有 solana-program 一份。
fn transfer(
    token_program: &Pubkey,
    from: &Pubkey,
    to: &Pubkey,
    authority: &Pubkey,
    amount: u64,
) -> Instruction {
    let mut data = Vec::with_capacity(9);
    data.push(3);
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: *token_program,
        accounts: vec![
            AccountMeta::new(*from, false),
            AccountMeta::new(*to, false),
            AccountMeta::new_readonly(*authority, true),
        ],
        data,
    }
}
