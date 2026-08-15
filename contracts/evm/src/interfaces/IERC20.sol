// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

/// @title IERC20
/// @author stablecoin-settlement-engine
/// @notice 依 EIP-20 規格從零撰寫的最小介面，只包含標準要求的六個函式與兩個事件。
/// @dev 規格來源：https://eips.ethereum.org/EIPS/eip-20
///
///      這裡刻意不引入任何函式庫的版本，因為整個 Day 2 的重點就在這份 ABI 上：
///      EIP-20 規定 transfer / transferFrom / approve 都必須回傳 bool，
///      但主網上市值最大的穩定幣（USDT）並沒有照做。
///      任何以這份介面撰寫的整合程式碼，碰到那類 token 時會在「解碼回傳值」的階段 revert。
///
///      name / symbol / decimals 在 EIP-20 裡屬於 OPTIONAL，因此不放進這份介面；
///      需要的地方各自宣告，避免整合時對不上簽章。
interface IERC20 {
    /// @notice 代幣轉移時發出。EIP-20 要求即使 _value 為 0 也必須發出。
    event Transfer(address indexed _from, address indexed _to, uint256 _value);

    /// @notice 授權額度變更成功時發出。
    event Approval(address indexed _owner, address indexed _spender, uint256 _value);

    /// @notice 代幣總發行量。
    function totalSupply() external view returns (uint256);

    /// @notice 查詢某位址的餘額。
    function balanceOf(address _owner) external view returns (uint256 balance);

    /// @notice 由 msg.sender 轉出 _value 給 _to。
    /// @return success 依 EIP-20 必須回傳 bool；呼叫端必須檢查它。
    function transfer(address _to, uint256 _value) external returns (bool success);

    /// @notice 動用 _from 給 msg.sender 的授權額度，把 _value 轉給 _to。
    /// @return success 依 EIP-20 必須回傳 bool；呼叫端必須檢查它。
    function transferFrom(address _from, address _to, uint256 _value) external returns (bool success);

    /// @notice 授權 _spender 可從 msg.sender 帳上動用 _value。
    /// @return success 依 EIP-20 必須回傳 bool；呼叫端必須檢查它。
    function approve(address _spender, uint256 _value) external returns (bool success);

    /// @notice 查詢 _owner 給 _spender 的剩餘授權額度。
    function allowance(address _owner, address _spender) external view returns (uint256 remaining);
}
