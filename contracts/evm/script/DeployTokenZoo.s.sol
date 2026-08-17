// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

import {Script, console} from "forge-std/Script.sol";
import {TokenZooBase} from "./TokenZooBase.sol";
import {DevnetAccounts} from "./DevnetAccounts.sol";

/// @title DeployTokenZoo
/// @notice 部署 Token Zoo，並把地址寫進 deployments/<chainId>.json。
/// @dev 用法：forge script script/DeployTokenZoo.s.sol --rpc-url anvil --broadcast
///      有設 DEPLOYER_KEY 就用它簽，否則用 Anvil 預設帳號 #0。
///      廣播區塊內只有部署，寫檔在廣播結束後才做，避免把讀寫檔的 cheatcode 混進交易收集階段。
contract DeployTokenZoo is Script, TokenZooBase {
    function run() external returns (Zoo memory zoo) {
        uint256 deployerKey = vm.envOr("DEPLOYER_KEY", DevnetAccounts.key(DevnetAccounts.DEPLOYER));
        address deployer = vm.addr(deployerKey);

        vm.startBroadcast(deployerKey);
        zoo = deploy(deployer);
        vm.stopBroadcast();

        vm.writeJson(toJson(zoo, deployer), deploymentsPath());
        console.log("Token Zoo deployed by %s", deployer);
        console.log("Addresses written to %s", deploymentsPath());
    }
}
