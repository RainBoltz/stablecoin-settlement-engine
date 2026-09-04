package bulk

// Rule 是一條鏈上某一種資源的算法：一筆交易先付 Base，每多一項付款再付 Item，
// 帶著樹的批（Align 模式）每一層證明再付 PerLevel，加起來不能超過 Cap。
//
// 加項都是線性的，因為一筆交易被序列化之後本來就是線性的：多一項付款就是多一個地址、
// 多一個索引、多一組參數；證明多一層就是多一個雜湊。真正非線性的東西（合約內部怎麼跑）
// 不歸這裡管，那是 gas 的事。
type Rule struct {
	// Unit 是這種資源的名字，只拿來印報告。
	Unit string
	Cap  uint64
	Base uint64
	Item uint64
	// PerLevel 是 Align 模式下，樹每比一批的區塊高一層，這一批要多付的量：
	// 證明一層一個雜湊，所以 bytes 那條是 32，數帳戶的那條是 0。
	PerLevel uint64
	// Source 是這個上限的公開出處。每一個數字都要有人能查，這條規則跟文章一樣。
	Source string
}

// Limits 是一條鏈的一筆交易裝得下多少項付款。所有規則都要過，先撞上的那一條決定一批多大。
type Limits struct {
	Chain string
	Rules []Rule

	// Align 決定批的形狀。0 是貪心切法：塞得下就多塞一項，塞不下換一批（EVM）。
	// 大於 0 時批對齊在 merkle 樹的區塊上，一批最多 Align 項、切在 Align 的倍數邊界：
	// payer 簽的 root 蓋住整份名單，對齊的區塊才共用得起一份「區塊走回 root」的證明。
	// Align 必須是 2 的冪次，樹是二元的。
	Align uint64

	// PrepareRules 是「送錢之前先開帳戶」那種交易的規則；空的話這條鏈沒有 prepare 階段。
	// 開帳戶批的內容跟付款批完全不同（它列的是 merchant 本人與新帳戶，不帶金額與 ref），
	// 所以它有自己的一組 Base 與 Item，跟付款批的規則各算各的。
	PrepareRules []Rule

	// NewAccountRent 是替一個 merchant 開 token 帳戶要先墊的錢，用這條鏈原生代幣的最小單位。
	// 這筆錢不是手續費：它鎖在那個帳戶裡，帳戶關掉才拿得回來。
	NewAccountRent uint64
	RentUnit       string
}

// Defaults 是四條鏈裡目前有實作的那兩條。其他兩條之後再補。
//
// 兩條鏈的規則數不一樣，這是刻意的：EVM 上一筆交易裝得下多少完全由 gas 決定，
// Solana 上 gas 這種東西不存在，換成「交易本身有多長」與「它列出了幾個帳戶」兩個各自獨立的上限。
// Solana 這裡的 Base 與 Item 是照 pay_batch 那種交易的形狀逐個欄位加出來的估算，
// 每一個加項都寫在註解裡；真正序列化出來的長度以送出前組好的那筆為準，這裡寧可高估。
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
				Base:     37_310,
				Item:     53_220,
				PerLevel: 0,
				Source:   "https://ethereum.org/en/developers/docs/blocks/#block-size",
			}},
			// EVM 上沒有 rent 這回事：payee 第一次拿到一顆 token 不需要有人先替他開帳戶。
			NewAccountRent: 0,
			RentUnit:       "wei",
		},
		"solana": {
			Chain: "solana",
			// 一批 8 項：對齊區塊要是 2 的冪次，16 項的 bytes 就超過 1,232（16 x 73 加上
			// 固定開銷與證明，怎麼算都在 1,600 以上），所以 8 是塞得進一筆交易的最大冪次。
			Align: 8,
			Rules: []Rule{
				{
					Unit: "bytes",
					// 「The total serialized size of a transaction must not exceed PACKET_DATA_SIZE (1,232 bytes).」
					// 這個數字是 IPv6 的最小 MTU 1,280 減掉 48 bytes 的表頭，跟程式寫得多好無關。
					Cap: 1232,
					// Base 是一筆 pay_batch 不管裝幾項都要付的：一個簽名 65（relayer 是唯一
					// 的簽名者，payer 簽的是 root 不是這筆交易）、message 表頭 3、帳戶清單
					// 1 + 5 個固定帳戶各 32（付手續費的錢包、run PDA、vault 的 token 帳戶、
					// SPL Token 程式、我們的程式）、recent blockhash 32、指令表頭與固定參數 19
					//（指令數 1、程式索引 1、帳戶索引 3 加長度 1、資料長度 2、discriminator 8、
					// 區塊編號 2、項數 1，資料長度用最保守的 2 bytes 算）。
					Base: 280,
					// Item 是每多一項付款：merchant 的 token 帳戶地址 32、它在指令裡的索引 1、
					// 金額 8、還有我們自己那把 32 bytes 的 ref。
					Item: 73,
					// PerLevel 是證明的一層：一個雜湊 32 bytes。整批共用一份證明，
					// 層數是樹高減掉區塊自己的 3 層。
					PerLevel: 32,
					Source:   "https://solana.com/docs/core/transactions/transaction-structure",
				},
				{
					Unit: "accounts",
					// 執行期一筆交易最多載入 64 個帳戶，地址搬進 lookup table 也一樣。
					Cap: 64,
					// 固定的五個：付手續費的錢包、run PDA、vault 的 token 帳戶、
					// SPL Token 程式、我們的程式。
					Base:     5,
					Item:     1,
					PerLevel: 0,
					Source:   "https://solana.com/docs/advanced/lookup-tables",
				},
			},
			// prepare batch 開的是 associated token account，一筆交易裡一個 merchant 一段
			// CreateIdempotent 指令。https://www.solana-program.com/docs/associated-token-account
			PrepareRules: []Rule{
				{
					Unit: "bytes",
					Cap:  1232,
					// 固定：簽名 65、表頭 3、帳戶清單 1 + 5 個固定帳戶各 32（付手續費同時
					// 出 rent 的錢包、mint、System 程式、SPL Token 程式、ATA 程式）、
					// blockhash 32、指令數 1。
					Base: 262,
					// 每開一個帳戶：新帳戶與 merchant 本人各 32、一段 CreateIdempotent 指令
					// 10（程式索引 1、帳戶索引 6 加長度 1、資料長度 1、discriminator 1）。
					Item:     74,
					PerLevel: 0,
					Source:   "https://www.solana-program.com/docs/associated-token-account",
				},
				{
					Unit:     "accounts",
					Cap:      64,
					Base:     5,
					Item:     2,
					PerLevel: 0,
					Source:   "https://solana.com/docs/advanced/lookup-tables",
				},
			},
			// 一個 SPL token 帳戶是 165 bytes，照 (165 + 128) x 3,480 x 2 算出來要墊 2,039,280 lamports。
			// https://solana.com/docs/core/accounts
			NewAccountRent: 2_039_280,
			RentUnit:       "lamports",
		},
	}
}

// MaxItems 回報「一批最多能塞幾項」，用「不帶證明」的最鬆算法。
// Pack 不用它：Align 模式下一批還要付證明的 bytes，證明幾層又由整份名單的長度決定，
// 這裡沒有名單可看，所以它給的是上界不是答案。它是給呼叫端估規模用的，切批交給 Pack。
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
	if l.Align > 0 && best > int(l.Align) {
		best = int(l.Align)
	}
	return best
}
