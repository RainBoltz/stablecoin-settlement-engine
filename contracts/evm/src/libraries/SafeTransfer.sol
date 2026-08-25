// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {IERC20} from "../interfaces/IERC20.sol";

/// @title SafeTransfer
/// @notice 搬錢的安全封裝：把 token 對一筆轉帳的三種真實回應（revert、回傳 false、
///         什麼都不回）收斂成同一種語義：沒成功就 revert。
/// @dev 本 library 為本系列從零設計，只取公開設計裡需要的那部分。
///      業界的標準答案是 OpenZeppelin 的 SafeERC20
///      （https://docs.openzeppelin.com/contracts/5.x/api/token/erc20#SafeERC20），
///      這裡照它公開文件描述的行為實作最小子集：只包 transfer 與 transferFrom
///      這兩個搬錢動作。approve 的封裝（forceApprove 之類）刻意不包，
///      因為這個 repo 的合約不對任何人 approve，包了就是一段沒有呼叫者的程式碼。
///
///      為什麼標準介面走不下去：IERC20 宣告了 returns (bool)，Solidity 會在呼叫後
///      解碼 returndata，碰到不回傳值的 token（主網 USDT）時死在解碼，連成功的轉帳
///      都過不了。反過來把介面宣告成沒有回傳值，又會丟掉「回傳 false」這個失敗訊號。
///      同時容納 0 bytes 與 32 bytes 兩種 returndata 的方法只剩一種：低階 call，
///      自己讀 returndata、自己決定每一種長度是什麼意思。
///
///      低階 call 的代價是丟掉編譯器送的保護：對一個沒有程式碼的地址 call 永遠成功，
///      returndata 是 0 bytes，跟 USDT 轉帳成功的樣子完全相同。所以 0 bytes 那條路
///      必須自己補一次 code 檢查，不然打錯 token 地址會變成一筆「成功」的空轉帳。
library SafeTransfer {
    /// @notice 把呼叫端名下的 token 轉給 to，沒成功就 revert。
    function safeTransfer(IERC20 token, address to, uint256 value) internal {
        _mustSucceed(address(token), abi.encodeCall(IERC20.transfer, (to, value)));
    }

    /// @notice 動用 from 給呼叫端的 allowance 把 token 轉給 to，沒成功就 revert。
    function safeTransferFrom(IERC20 token, address from, address to, uint256 value) internal {
        _mustSucceed(address(token), abi.encodeCall(IERC20.transferFrom, (from, to, value)));
    }

    /// @dev 唯一的判讀路徑，兩個搬錢動作共用。三種回應各有一條出路：
    ///
    ///      1. call 失敗：把 token 自己的 revert 原因原封不動轉發出去，不蓋成我們的字串。
    ///         「是誰、為什麼擋下這筆轉帳」是鏈下把失敗分類成 retryable 或 poison 的線索，
    ///         蓋掉它，relayer 看到的每一種失敗就都長一樣了。
    ///      2. call 成功、returndata 是 0 bytes：USDT 這類不回傳值的 token 成功時長這樣，
    ///         此時要補查 token 有沒有 code。檢查放在這條路裡而不是每次呼叫都做，
    ///         因為回得出資料的地址一定有程式碼，只有 0 bytes 這條路需要花這筆 gas。
    ///      3. call 成功、有 returndata：依 EIP-20 解成 bool
    ///         （https://eips.ethereum.org/EIPS/eip-20），false 轉成明確的 revert，
    ///         「交易成功、錢沒動」的幽靈支付在這一行變回一筆看得見的失敗。
    function _mustSucceed(address token, bytes memory data) private {
        (bool success, bytes memory returndata) = token.call(data);
        if (!success) {
            // 轉發 returndata：token 帶什麼理由就丟回什麼理由；長度為 0 時等同不帶理由的 revert
            assembly ("memory-safe") {
                revert(add(returndata, 0x20), mload(returndata))
            }
        }
        if (returndata.length == 0) {
            require(token.code.length > 0, "SafeTransfer: token has no code");
            return;
        }
        require(abi.decode(returndata, (bool)), "SafeTransfer: transfer returned false");
    }
}
