// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

/// @title ISignatureTransfer
/// @author stablecoin-settlement-engine
/// @notice Permit2 兩半功能裡「一次性簽名轉帳」那一半的最小介面，依公開文件從零撰寫。
/// @dev 規格來源：https://docs.uniswap.org/contracts/permit2/reference/signature-transfer
///
///      Permit2 是 Uniswap 部署的單一合約，用 CREATE2 讓每條鏈上的地址都相同
///      （0x000000000022D473030F116dDEE9F6B43aC78BA3）。它替所有 ERC-20 補上
///      簽名授權：付款人只要對 Permit2 做過一次 approve，之後每一筆轉帳都能用
///      離線簽名代替鏈上交易，token 本身不必實作 EIP-2612。
///
///      這裡只宣告 permitWitnessTransferFrom 這條路徑。另一半 AllowanceTransfer
///      仍然是一份存在鏈上、帶到期時間的額度，一筆付款用不到；
///      SignatureTransfer 的一份簽名只搬得動一次錢，跟一筆付款是一對一。
///
///      witness 是這份介面存在的理由：不帶 witness 的簽名裡沒有收款人，
///      permit 只約束「哪一顆 token、多少、給哪個 spender、什麼時候之前」，
///      收款地址是 spender 呼叫時自己填的。witness 讓呼叫端把自訂欄位
///      （本專案是 PaymentRef 與 merchant）一起放進付款人簽的那份 EIP-712 資料。
interface ISignatureTransfer {
    /// @notice 被授權動用的 token 與上限金額。
    struct TokenPermissions {
        address token;
        uint256 amount;
    }

    /// @notice 付款人簽下的授權本體。spender 不在結構裡：Permit2 驗簽時用 msg.sender 補上。
    struct PermitTransferFrom {
        TokenPermissions permitted;
        uint256 nonce;
        uint256 deadline;
    }

    /// @notice 呼叫端指定的收款人與實際請求金額，requestedAmount 不得超過 permitted.amount。
    /// @dev 這兩個欄位都不在簽名裡，所以「錢會進誰的口袋」要靠 witness 綁住。
    struct SignatureTransferDetails {
        address to;
        uint256 requestedAmount;
    }

    /// @notice EIP-712 的 domain separator，簽名端要拿它組出 digest。
    function DOMAIN_SEPARATOR() external view returns (bytes32);

    /// @notice 某個位址第 word 個 nonce 字組的使用狀況，一個 bit 代表一個 nonce。
    function nonceBitmap(address owner, uint256 word) external view returns (uint256);

    /// @notice 驗證付款人的簽名，並在同一筆交易裡把 token 從 owner 轉給 transferDetails.to。
    /// @param witness 呼叫端自訂欄位的 hash，會被算進付款人簽的 EIP-712 結構。
    /// @param witnessTypeString witness 的型別字串，格式是
    ///        "<型別名> witness)<型別定義>TokenPermissions(address token,uint256 amount)"。
    function permitWitnessTransferFrom(
        PermitTransferFrom memory permit,
        SignatureTransferDetails calldata transferDetails,
        address owner,
        bytes32 witness,
        string calldata witnessTypeString,
        bytes calldata signature
    ) external;
}
