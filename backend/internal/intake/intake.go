// Package intake 把一份 CSV 撥款檔案讀成一份可以送出去的名單，並且交代它剔掉了哪幾行。
//
// 結算合約的批次入口是全成或全敗：一批裡有一項搬不動，整筆交易回滾，連帶把同一批
// 其他人的錢也退回去。所以「部分失敗」這件事不能留到鏈上才處理，一行有問題的資料
// 走到鏈上的時候，賠的是跟它同一批的另外十一個 merchant。
//
// 這個 package 因此只做一件事：在任何東西被簽名之前，把檔案分成「可以送」與「不能送」兩堆，
// 而且第二堆的每一項都要說得出是第幾行、哪一欄、為什麼。它不組交易、不切批、不認識 RPC，
// 也不判斷 merchant 的地址在目標鏈上長得對不對，那是每條鏈各自的規則，之後再談。
//
// 剔掉的行要留住行號，是因為出事之後要回頭跟人交代的是那份檔案，不是我們的資料結構。
// 同一個理由讓 Run 記住每一項付款來自第幾行：批次在鏈上失敗的時候，operator 要看的是
// 「檔案第 87 到 99 行沒有付出去」；「第 8 批失敗了」這句話在那份檔案上找不到對應。
//
// 本 package 為本系列從零設計，只取公開設計裡需要的那部分。
package intake

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
)

// Terms 是整份檔案共用的那幾件事：這一批要付到哪條鏈、付哪一顆 token、誰出錢。
//
// 這三樣刻意不放進 CSV 的欄位裡。一行只放「這一行才有的東西」，好處是整整一類的錯誤
// 不可能發生：檔案裡沒有一行有辦法指定另一顆 token 或另一個 payer。
type Terms struct {
	Chain string
	Token string
	Payer string
}

// Reason 是一行被剔掉的原因。名字直接用欄位名，因為報告要交給的是編出這份檔案的人，
// 他手上有的是那份檔案，不是我們的型別。
type Reason string

const (
	// ReasonFields：這一行的欄位數不對，或者引號沒有收好。這一關由 CSV 的格式本身決定，
	// 還輪不到我們檢查內容。
	ReasonFields Reason = "fields"
	// ReasonIntentID：intent id 空白、太長，或含有不該有的字元。
	ReasonIntentID Reason = "intent_id"
	// ReasonMerchant：merchant 空白或太長。
	ReasonMerchant Reason = "merchant"
	// ReasonAmount：金額不是一個正整數的最小單位。
	ReasonAmount Reason = "amount"
	// ReasonDuplicate：同一個 intent id 在這份檔案裡出現第二次。
	//
	// 這一關擋的跟合約那一關擋的不是同一件事：合約擋的是同一把 ref 被取用兩次，
	// 而同一個 intent 用兩個不同的金額寫兩行，會算出兩把不同的 ref，合約全部放行。
	ReasonDuplicate Reason = "duplicate"
)

// Reject 是被剔掉的一行。四個欄位剛好回答 operator 會問的四件事：
// 哪一行、哪一欄、為什麼、原本寫的是什麼。
type Reject struct {
	Line   int
	Reason Reason
	Detail string
}

// Policy 是「這份檔案有幾行壞掉的時候怎麼收」，由呼叫端決定，因為決定它的是業務規則。
type Policy struct {
	// Skip 打開的話就把壞的行剔掉、其餘照送；預設是關的，一行壞掉整份退回。
	//
	// 預設值選整份退回，是因為撥款檔案多半是另一個系統產生的：某一行不合格通常是
	// 產生它的那一段壞了，很少是那一個 merchant 本身特別。這種時候先把其餘的付掉，
	// 等於把一個還沒查清楚的錯誤變成一筆已經送出去的錢。
	Skip bool

	// MaxRejects 是 Skip 打開之後還能容忍幾行，0 代表不設上限。
	// 上限用絕對數字不用比例：一份三行的檔案壞掉一行是三成三，一份三千行的檔案壞掉一行是萬分之三，
	// 但要人去看的工作量是一樣的。
	MaxRejects int
}

// Run 是讀完一份檔案的結果：可以送的那些、被剔掉的那些，以及每一項來自第幾行。
type Run struct {
	Chain string
	// Rows 是檔案裡的資料行數，不含表頭。
	Rows     int
	Accepted []bulk.Payout
	// Lines 跟 Accepted 一樣長：第 i 項付款來自檔案第 Lines[i] 行。
	// 中間有行被剔掉之後，這個對應算不出來，只能記著。
	Lines    []int
	Rejected []Reject
}

// Trace 是「第幾批對到檔案第幾行」的答案，一批在鏈上失敗之後就靠它回頭交代。
type Trace struct {
	Batch int
	Lines []int
}

// ErrNoRows：表頭以外一行資料都沒有。空名單在 bulk 那一側也會被擋，這裡先擋掉比較好交代。
var ErrNoRows = errors.New("intake: the file has no payout rows")

// ErrBadHeader：第一行不是我們認得的表頭。表頭對不上的時候每一行的欄位意義都是猜的，
// 猜錯的下場是把金額付給一個看起來像 merchant 的字串，所以這裡不做任何容錯。
var ErrBadHeader = errors.New("intake: the header row is not intent_id,merchant,amount")

// ErrRejected：有行被剔掉，而這份 Policy 不跳過。
var ErrRejected = errors.New("intake: rejected rows and the policy does not skip them")

// ErrTooManyRejects：跳過是開的，但壞掉的行數超過 MaxRejects。
var ErrTooManyRejects = errors.New("intake: too many rejected rows to skip")

// ErrPlanMismatch：這份計畫不是拿這份 Run 切出來的，行號對不過去。
var ErrPlanMismatch = errors.New("intake: the plan was not packed from this run")

// ErrNoSuchBatch：計畫裡沒有這一批。
var ErrNoSuchBatch = errors.New("intake: the plan has no such batch")

// String 印一份 Run 的總結行。格式固定，Example 與文章會直接貼這一行。
func (r Run) String() string {
	return fmt.Sprintf("intake  %-8s %d rows  %d accepted  %d rejected",
		r.Chain, r.Rows, len(r.Accepted), len(r.Rejected))
}

// String 印一行被剔掉的紀錄。行號靠左對齊，因為報告是一行一行往下讀的。
func (rj Reject) String() string {
	return fmt.Sprintf("reject  line %-5d %-10s %s", rj.Line, rj.Reason, rj.Detail)
}

// String 印一批對到的行號。連號的收成範圍，因為中間被剔掉幾行之後這串數字會有缺口，
// 而那個缺口正是 operator 需要看見的東西。
func (t Trace) String() string {
	return fmt.Sprintf("trace   batch #%-3d %d items  csv lines %s", t.Batch, len(t.Lines), ranges(t.Lines))
}

// ranges 把一串遞增的行號收成 "2-13" 或 "87-88, 90-99"。
func ranges(lines []int) string {
	if len(lines) == 0 {
		return "none"
	}
	var out []string
	start, prev := lines[0], lines[0]
	flush := func() {
		if start == prev {
			out = append(out, strconv.Itoa(start))
			return
		}
		out = append(out, strconv.Itoa(start)+"-"+strconv.Itoa(prev))
	}
	for _, l := range lines[1:] {
		if l == prev+1 {
			prev = l
			continue
		}
		flush()
		start, prev = l, l
	}
	flush()
	return strings.Join(out, ", ")
}
