package chain_test

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/chain"
)

// TestBuild_TheSameRefsRideBothChains 是兩個 builder 之間唯一的共同義務：同一份名單，
// 每一把 ref 都要一字節不差地在兩邊的輸出裡各出現一次。EVM 把 5 筆收成一份 calldata、
// Solana 把它們收成一輪一個區塊的 run，版面沒有一個 byte 相同，listener 能在兩條鏈上
// 用同一把 ref 對回同一筆 intent，靠的就是這條。
func TestBuild_TheSameRefsRideBothChains(t *testing.T) {
	items, accounts := solanaRun(5)

	calldata, err := chain.NewEVM().SettleBatchCalldata(evmToken, evmPayer, items)
	if err != nil {
		t.Fatalf("SettleBatchCalldata: %v", err)
	}
	run, err := chain.NewSolana().NewRun(items, accounts)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	msg, err := run.PayBatchMessage(runAccounts(), sha256.Sum256([]byte("recent-blockhash")), 0)
	if err != nil {
		t.Fatalf("PayBatchMessage: %v", err)
	}

	for i, it := range items {
		if n := bytes.Count(calldata, it.Ref[:]); n != 1 {
			t.Fatalf("ref %d appears %d times in the calldata, want once", i, n)
		}
		if n := bytes.Count(msg, it.Ref[:]); n != 1 {
			t.Fatalf("ref %d appears %d times in the message, want once", i, n)
		}
	}
}
