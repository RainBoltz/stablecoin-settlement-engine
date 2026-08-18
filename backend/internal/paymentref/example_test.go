package paymentref_test

import (
	"fmt"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// Example_derive 是 PaymentRef 長什麼樣：同一組條件算出同一個 ref，金額差 1 個最小單位就是另一個 ref。
// 拿著 intent 的任何人都能自己算，不用問伺服器。
func Example_derive() {
	terms := paymentref.Terms{
		IntentID: "pi_0001",
		Chain:    "evm:31337",
		Token:    "0x5FbDB2315678afecb367f032d93F642f64180aa3", // devnet 的 USDC
		Payer:    "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", // payer
		Merchant: "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", // merchant
		Amount:   "100000000",                                  // 100 USDC
	}
	fmt.Println("amount=100000000 ", paymentref.Derive(terms))
	fmt.Println("amount=100000000 ", paymentref.Derive(terms), "(again)")
	terms.Amount = "100000001"
	fmt.Println("amount=100000001 ", paymentref.Derive(terms))

	// Output:
	// amount=100000000  0xb02f8d2972380c471030066cf638083d0d6e1674d250a38f2347c28fc5783c47
	// amount=100000000  0xb02f8d2972380c471030066cf638083d0d6e1674d250a38f2347c28fc5783c47 (again)
	// amount=100000001  0xd694564081abc8e053640301fd99658865c74a0328bac8b664268eafc21c16fe
}
