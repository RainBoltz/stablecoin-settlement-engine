package bulk

// Rule 是一條鏈上某一種資源的算法：一筆交易先付 Base，每多一項付款再付 Item，
// 那一項還要替 merchant 開 token 帳戶的話另外加 Extra，加起來不能超過 Cap。
//
// 三個加項都是線性的，因為一筆交易被序列化之後本來就是線性的：多一項付款就是多一個地址、
// 多一個索引、多一組參數。真正非線性的東西（合約內部怎麼跑）不歸這裡管，那是 gas 的事。
type Rule struct {
	// Unit 是這種資源的名字，只拿來印報告。
	Unit string
	Cap  uint64
	Base uint64
	Item uint64
	// Extra 是「這一項還要先替 merchant 開一個 token 帳戶」的加價。
	Extra uint64
	// Source 是這個上限的公開出處。每一個數字都要有人能查，這條規則跟文章一樣。
	Source string
}

// Limits 是一條鏈的一筆交易裝得下多少項付款。所有規則都要過，先撞上的那一條決定一批多大。
type Limits struct {
	Chain string
	Rules []Rule

	// NewAccountRent 是替一個 merchant 開 token 帳戶要先墊的錢，用這條鏈原生代幣的最小單位。
	// 這筆錢不是手續費：它鎖在那個帳戶裡，帳戶關掉才拿得回來。
	NewAccountRent uint64
	RentUnit       string
}

// Defaults 是四條鏈裡目前有實作的那兩條。其他兩條之後再補。
//
// 兩條鏈的規則數不一樣，這是刻意的：EVM 上一筆交易裝得下多少完全由 gas 決定，
// Solana 上 gas 這種東西不存在，換成「交易本身有多長」與「它列出了幾個帳戶」兩個各自獨立的上限。
func Defaults() map[string]Limits {
	return map[string]Limits{
		"evm": {
			Chain: "evm",
			Rules: []Rule{{
				Unit: "gas",
				// 用 target block size 30M，不用 60M 的 block limit：target 是常態，
				// limit 是網路壅塞時才撐得開的彈性上限，撥款沒有理由去搶那段空間。
				// https://ethereum.org/en/developers/docs/blocks/#block-size
				Cap: 30_000_000,
				// Base 與 Item 是從結算合約的批次入口實跑的 gas report 反推的：
				// 一項 90,530、兩百項 10,681,008，所以每多一項約 53,220，固定開銷約 37,310。
				Base:   37_310,
				Item:   53_220,
				Extra:  0,
				Source: "https://ethereum.org/en/developers/docs/blocks/#block-size",
			}},
			// EVM 上沒有 rent 這回事：payee 第一次拿到一顆 token 不需要有人先替他開帳戶。
			NewAccountRent: 0,
			RentUnit:       "wei",
		},
		"solana": {
			Chain: "solana",
			Rules: []Rule{
				{
					Unit: "bytes",
					// 「The total serialized size of a transaction must not exceed PACKET_DATA_SIZE (1,232 bytes).」
					// 這個數字是 IPv6 的最小 MTU 1,280 減掉 48 bytes 的表頭，跟合約寫得多好無關。
					Cap: 1232,
					// Base 是一筆交易不管裝幾項都要付的：一個簽名 65、message 表頭 3、
					// 帳戶清單 1 + 6 個固定帳戶各 32、recent blockhash 32、指令表頭與參數 18。
					Base: 311,
					// Item 是每多一項付款：merchant 的 token 帳戶地址 32、它在指令裡的索引 1、
					// 金額 8、還有我們自己那把 32 bytes 的 ref。
					Item: 73,
					// Extra 是那一項還要先開帳戶：多一個 merchant 本人的地址 32，加一段開帳戶的指令。
					Extra:  42,
					Source: "https://solana.com/docs/core/transactions/transaction-structure",
				},
				{
					Unit: "accounts",
					// 執行期一筆交易最多載入 64 個帳戶，地址搬進 lookup table 也一樣。
					Cap: 64,
					// 固定的六個：付手續費的錢包、payer 的 token 帳戶、代簽的 PDA、
					// 我們的程式、SPL Token 程式、mint。
					Base:   6,
					Item:   1,
					Extra:  1,
					Source: "https://solana.com/docs/advanced/lookup-tables",
				},
			},
			// 一個 SPL token 帳戶是 165 bytes，照 (165 + 128) x 3,480 x 2 算出來要墊 2,039,280 lamports。
			// https://solana.com/docs/core/accounts
			NewAccountRent: 2_039_280,
			RentUnit:       "lamports",
		},
	}
}

// MaxItems 回報「這一批最多還能塞幾項」，前提是接下來每一項都不用開帳戶。
// Pack 不用它（Pack 是一項一項試的，因為每一項不一樣貴），它是給呼叫端估規模用的。
func (l Limits) MaxItems() int {
	best := -1
	for _, r := range l.Rules {
		if r.Item == 0 {
			continue
		}
		if r.Cap < r.Base {
			return 0
		}
		n := int((r.Cap - r.Base) / r.Item)
		if best < 0 || n < best {
			best = n
		}
	}
	if best < 0 {
		return 0
	}
	return best
}
