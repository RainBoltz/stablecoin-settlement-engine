// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Vm} from "forge-std/Vm.sol";

/// @title DevnetAccounts
/// @notice 整個系列的本地角色表。之後每一篇提到 payer、merchant、relayer，指的都是這裡的地址。
/// @dev 全部從 Anvil 的預設助記詞推導，不寫死 hex：Solidity 測試、shell 腳本、之後的 Go 後端
///      算出來的都是同一批人。這組助記詞是公開的，只能對著本地 Anvil 用；Foundry 文件也提醒
///      fork 主網或接公開 RPC 時不要用預設帳號：https://getfoundry.sh/anvil/overview
library DevnetAccounts {
    Vm private constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));

    string internal constant MNEMONIC = "test test test test test test test test test test test junk";

    // Anvil 預設帳號表的 index
    uint32 internal constant DEPLOYER = 0; // 部署 Token Zoo、擁有 USDTMock（扮演發行商）
    uint32 internal constant PAYER = 1; // 錢會動的那個人
    uint32 internal constant MERCHANT = 2; // 收款方
    uint32 internal constant RELAYER = 3; // 之後替人代付 gas 的錢包
    uint32 internal constant BLACKLISTED = 9; // 永遠失敗的對手方

    struct Cast {
        address deployer;
        address payer;
        address merchant;
        address relayer;
        address blacklisted;
    }

    function key(uint32 index) internal pure returns (uint256) {
        return vm.deriveKey(MNEMONIC, index);
    }

    function addr(uint32 index) internal pure returns (address) {
        return vm.addr(key(index));
    }

    function cast() internal pure returns (Cast memory c) {
        c.deployer = addr(DEPLOYER);
        c.payer = addr(PAYER);
        c.merchant = addr(MERCHANT);
        c.relayer = addr(RELAYER);
        c.blacklisted = addr(BLACKLISTED);
    }
}
