// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "../../src/interfaces/IERC20.sol";
import {ISignatureTransfer} from "../../src/interfaces/ISignatureTransfer.sol";
import {SafeTransfer} from "../../src/libraries/SafeTransfer.sol";

/// @title Permit2Mock
/// @notice Permit2 的離線替身：只實作 permitWitnessTransferFrom 這一條路徑，
///         但 EIP-712 的每一個欄位都照公開規格算，所以簽名能不能過的答案跟主網一致。
/// @dev 依公開文件從零撰寫，規格來源：
///      https://docs.uniswap.org/contracts/permit2/reference/signature-transfer
///
///      它不住在 src/mocks/：那裡是 token 動物園，收的是真實世界的 ERC-20 例外行為，
///      而這顆是另一份協定的替身，只有測試用得到它。
///      主網上真正的 Permit2 是 Uniswap 部署的單一合約，
///      test/fork/SettlementPermit2Mainnet.t.sol 會拿同一份簽名對它重放一次。
///
///      驗簽以外的每一件事都刻意照抄行為而不是照抄程式碼：
///      - domain 的 name 是 "Permit2"、沒有 version 欄位，所以 digest 才跟主網相同。
///      - nonce 是無序的 bitmap：nonce >> 8 選字組、低 8 位選 bit，任何沒用過的
///        數字都可以，跟鏈上帳戶那條必須連號的 nonce 是相反的設計。
///      - 檢查順序是 deadline、金額、nonce、簽名，錯誤才會跟主網對得上。
contract Permit2Mock {
    using SafeTransfer for IERC20;

    bytes32 private constant TOKEN_PERMISSIONS_TYPEHASH = keccak256("TokenPermissions(address token,uint256 amount)");

    /// @dev Permit2 用這段前綴接上呼叫端給的 witness 型別字串，拼出完整的 EIP-712 typehash。
    string private constant WITNESS_TYPEHASH_STUB =
        "PermitWitnessTransferFrom(TokenPermissions permitted,address spender,uint256 nonce,uint256 deadline,";

    /// @notice 每個位址的 nonce 使用紀錄，一個 bit 一個 nonce。
    mapping(address => mapping(uint256 => uint256)) public nonceBitmap;

    error InvalidNonce();
    error SignatureExpired(uint256 signatureDeadline);
    error InvalidSigner();
    error InvalidAmount(uint256 maxAmount);
    error InvalidSignatureLength();

    /// @notice EIP-712 domain separator。動態算而不是存成 immutable，這樣把 code 搬到
    ///         別的位址上也算得出正確的值。
    function DOMAIN_SEPARATOR() public view returns (bytes32) {
        return keccak256(
            abi.encode(
                keccak256("EIP712Domain(string name,uint256 chainId,address verifyingContract)"),
                keccak256("Permit2"),
                block.chainid,
                address(this)
            )
        );
    }

    /// @notice 驗證 owner 的簽名，然後把 token 從 owner 搬給 transferDetails.to。
    /// @dev spender 不是參數：它就是 msg.sender。簽名裡綁的是「誰可以動這筆錢」，
    ///      所以別人拿到同一份簽名也用不了。
    function permitWitnessTransferFrom(
        ISignatureTransfer.PermitTransferFrom memory permit,
        ISignatureTransfer.SignatureTransferDetails calldata transferDetails,
        address owner,
        bytes32 witness,
        string calldata witnessTypeString,
        bytes calldata signature
    ) external {
        if (block.timestamp > permit.deadline) revert SignatureExpired(permit.deadline);
        if (transferDetails.requestedAmount > permit.permitted.amount) {
            revert InvalidAmount(permit.permitted.amount);
        }

        _useUnorderedNonce(owner, permit.nonce);

        bytes32 typeHash = keccak256(abi.encodePacked(WITNESS_TYPEHASH_STUB, witnessTypeString));
        bytes32 structHash = keccak256(
            abi.encode(
                typeHash,
                keccak256(abi.encode(TOKEN_PERMISSIONS_TYPEHASH, permit.permitted.token, permit.permitted.amount)),
                msg.sender,
                permit.nonce,
                permit.deadline,
                witness
            )
        );
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", DOMAIN_SEPARATOR(), structHash));

        if (_recover(digest, signature) != owner) revert InvalidSigner();

        IERC20(permit.permitted.token).safeTransferFrom(owner, transferDetails.to, transferDetails.requestedAmount);
    }

    /// @dev 用掉一個 nonce。翻 bit 之後如果原本就是 1，代表這個號碼用過了。
    function _useUnorderedNonce(address from, uint256 nonce) private {
        uint256 bit = 1 << uint8(nonce);
        uint256 flipped = nonceBitmap[from][nonce >> 8] ^= bit;
        if (flipped & bit == 0) revert InvalidNonce();
    }

    function _recover(bytes32 digest, bytes calldata signature) private pure returns (address) {
        if (signature.length != 65) revert InvalidSignatureLength();
        bytes32 r = bytes32(signature[0:32]);
        bytes32 s = bytes32(signature[32:64]);
        uint8 v = uint8(signature[64]);
        return ecrecover(digest, v, r, s);
    }
}
