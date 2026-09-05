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

// Defaults 是四條鏈各自的上限，以協定名為 key。
//
// 四條鏈的規則數不一樣，這是刻意的：EVM 上一筆交易裝得下多少完全由 gas 決定，
// Solana 上 gas 這種東西不存在，換成「交易本身有多長」與「它列出了幾個帳戶」兩個各自獨立的上限；
// TON 上限制的根本不是付款，是 relayer 送給錢包合約的那一則 external message：裝幾則 message、幾個 bytes、
// cell 樹幾層深，三條各自獨立；SUI 上一批就是一個 PTB，裝幾個 command、幾個 bytes、生出幾個 object，
// 同樣三條各自獨立。Solana 與 SUI 這裡的 Base 與 Item 是照那種交易的形狀逐個欄位加出來的估算，
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
				// Base 與 Item 是從結算合約的 batch 入口實跑的 gas report 反推的：
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
		"ton": {
			Chain: "ton",
			// 貪心切法，跟 EVM 一樣：這條鏈上「一批」只是一則 external message 裝了幾則付款 message，
			// 沒有樹、沒有證明，也沒有整批一起成功或失敗這回事，每一則 message 送出去之後各走各的。
			Rules: []Rule{
				{
					Unit: "messages",
					// W5 錢包一則請求最多送 255 則：錢包對照表寫「Up to 255 per request」（v4 是 4），
					// 跟 TVM 對一筆交易 action list 的上限同一個數字，超過的是整筆錢包交易失敗
					//（exit code 33，https://docs.ton.org/tvm/exit-codes），不是前 255 則照送。
					Cap:      255,
					Base:     0,
					Item:     1,
					PerLevel: 0,
					Source:   "https://docs.ton.org/contracts/standard/wallets/comparison",
				},
				{
					Unit: "bytes",
					// 「Maximum external message size in bytes | 65535」：節點對一則 external message 序列化之後的長度
					// 上限。付款是 internal message，各自另外算，這條管的只有 relayer 那一則。
					Cap: 65535,
					// Base 是一則 external message 不裝任何付款也要付的：BoC 的表頭 20、signing cell（opcode、wallet_id、
					// valid_until、seqno 與兩個旗標 bit）21、空的動作清單 2，共 43 bytes；再加 512 bits 的簽名
					// 64 bytes 與 external message 表頭（收件的錢包地址、import_fee、兩個旗標 bit）35 bytes，共 142。
					Base: 142,
					// Item 是每多一則付款 message：一格動作 cell（tag 4、mode 1、兩個 ref）、一個 MessageRelaxed
					//（表頭、jetton wallet 地址、附上的 TON、四個零欄位）、一個 transfer body（op、query_id、
					// 金額、兩個地址、forward 金額）、一個 forward_payload（op 加 32 bytes 的 ref），
					// 序列化之後 190 到 194 bytes，cell 超過 255 個之後每個 ref 的索引多一個 byte，取多的那一端。
					Item:     194,
					PerLevel: 0,
					Source:   "https://docs.ton.org/foundations/limits",
				},
				{
					Unit: "depth",
					// 「Maximum external message depth | 512」：cell 樹最深幾層。動作清單是一條鏈結串列，
					// 一則 message 就多一層，所以這條也是線性的：signing cell 加最深那則 message 自己的三層是 3，
					// 之後一則加一層。255 則是 258 層，離 512 還遠，但它跟 bytes 一樣是獨立的上限，得各算各的。
					Cap:      512,
					Base:     3,
					Item:     1,
					PerLevel: 0,
					Source:   "https://docs.ton.org/foundations/limits",
				},
			},
			// TON 上沒有 rent 這種先墊的錢：merchant 沒有 jetton wallet 的話，我們的 jetton wallet 會在
			// 轉帳那一步順便部署它，儲存費從每一則 message 附上的 TON 裡出，有沒有帳戶每一則都一樣貴。
			NewAccountRent: 0,
			RentUnit:       "nanoton",
		},
		"sui": {
			Chain: "sui",
			// 貪心切法：一批就是一個 PTB，payer 簽一次、整個 PTB 一起成功或失敗
			//（「If one transaction command fails, the entire block fails and no effects from the commands
			// are applied.」，https://docs.sui.io/concepts/transactions/prog-txn-blocks），跟 EVM 的 settleBatch
			// 同一種形狀，但塞的是 command 不是 calldata。三條規則管的都是同一個 PTB。
			Rules: []Rule{
				{
					Unit: "commands",
					// 「A PTB can perform up to 1,024 unique operations in a single execution」：
					// protocol config 裡叫 max_programmable_tx_commands。
					Cap: 1024,
					// Base 是切 coin 的那一個 SplitCoins：一個 command 一次切出整批要付的每一顆。
					Base: 1,
					// Item 是每一筆付款一個 MoveCall：settlement::pay 帶著切好的那顆 coin。
					Item:     1,
					PerLevel: 0,
					Source:   "https://docs.sui.io/concepts/transactions/prog-txn-blocks",
				},
				{
					Unit: "bytes",
					// max_tx_size_bytes = 128 * 1024：序列化之後的 TransactionData 不能超過這個長度。
					Cap: 131072,
					// Base 是一個 PTB 不管付幾筆都要付的：TransactionData 與 kind 的 tag 各 1、inputs 與 commands
					// 的長度各算 2（超過 127 就兩個 byte）、Book 與 payer 那顆 coin 兩個 owned object 輸入各 75
					//（tag 2、id 32、version 8、digest 33）、SplitCoins 自己 6（tag 1、coin 引數 3、清單長度 2）、
					// sender 32、GasData 122（一個 gas coin 74、sponsor 32、price 8、budget 8）、expiration 1。
					Base: 317,
					// Item 是每多一筆付款：三個 pure 輸入（金額 10、merchant 34、ref 35）、SplitCoins 清單裡
					// 多一個引數 3、以及一個 MoveCall 108（tag 1、package 32、模組名 11、函式名 4、
					// 一個 type argument 45、四個引數 15：Book 與兩個 pure 各 3，切出來的那顆 coin 是
					// NestedResult 要 5）。
					Item:     190,
					PerLevel: 0,
					Source:   "https://github.com/MystenLabs/sui/blob/mainnet-v1.78.1/crates/sui-protocol-config/src/lib.rs",
				},
				{
					Unit: "objects",
					// max_num_new_move_object_ids = 2048：一筆交易最多生出這麼多新的 object id，
					// 同一份設定裡 max_num_transferred_move_object_ids 也是 2048。
					Cap: 2048,
					// Base 是 0：Book 與 payer 的 coin 都是既有的 object，改它們不生新的 id。
					Base: 0,
					// Item 是每一筆付款生出的兩個 object：SplitCoins 切給 merchant 的那顆 coin，
					// 加上 Book 的 Table 裡記這把 ref 的那個 dynamic field。
					Item:     2,
					PerLevel: 0,
					Source:   "https://github.com/MystenLabs/sui/blob/mainnet-v1.78.1/crates/sui-protocol-config/src/lib.rs",
				},
			},
			// SUI 上沒有替 merchant 開帳戶這件事：付過去的錢自己就是一個新的 Coin object，歸 merchant 所有。
			// 生出這個 object 的 storage 費用由這筆交易的 gas 出，object 之後被合併或刪掉時退 99%。
			NewAccountRent: 0,
			RentUnit:       "mist",
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
