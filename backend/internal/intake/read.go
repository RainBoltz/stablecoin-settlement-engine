package intake

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/bulk"
	"github.com/RainBoltz/stablecoin-settlement-engine/backend/internal/paymentref"
)

// header 是唯一認得的表頭。欄位只有三個，因為 chain、token 與 payer 是整份檔案的屬性，
// 由呼叫端用 Terms 傳進來。
var header = []string{"intent_id", "merchant", "amount"}

// maxField 是 intent id 與 merchant 的長度上限。設一個上限是為了讓「這一欄根本不是識別碼」
// 這種情況在檔案這一關就停下來，不要拖到變成一筆送不出去的交易。
const maxField = 64

// Read 讀一份 CSV 撥款檔案，回一份 Run。
//
// 三種失敗的處置刻意不一樣：
//
// 表頭不對或一行資料都沒有，整份檔案退回，沒有「部分成功」這回事，因為接下來每一行的
// 意義都是猜的。某一行不合格，那一行進 Rejected，整份檔案的去留交給 Policy。
// 至於一批在鏈上失敗，那已經是簽名之後的事，這裡管不到，Run.Trace 只負責讓它翻譯得回行號。
//
// 整份檔案退回的時候 Accepted 是空的，Rejected 照樣填滿：報告一定要交出去，
// 但漏看 error 的呼叫端最糟只會送出零筆，不會送出一半。
//
// Skip 打開而且每一行都被剔掉的時候，回的是一份零筆的 Run，沒有 error：讀檔這件事成功了，
// 這份檔案只是沒有東西可以送，而空名單在 bulk 那一側會被擋下來。
func Read(r io.Reader, t Terms, p Policy) (Run, error) {
	run := Run{Chain: t.Chain}

	cr := csv.NewReader(r)
	// 欄位數自己檢查，不交給 csv：它把欄位數不對當成整份檔案的錯誤回報，
	// 而我們要的是「這一行不合格」，其他行照讀。
	cr.FieldsPerRecord = -1

	first, err := cr.Read()
	if err == io.EOF {
		return run, ErrNoRows
	}
	if err != nil {
		return run, fmt.Errorf("%w: %v", ErrBadHeader, err)
	}
	// 試算表存出來的 CSV 開頭常常帶一個 BOM，那不是表頭寫錯。
	if len(first) > 0 {
		first[0] = strings.TrimPrefix(first[0], "\ufeff")
	}
	if !sameHeader(first) {
		return run, fmt.Errorf("%w: got %q", ErrBadHeader, strings.Join(first, ","))
	}

	seen := make(map[string]int)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 引號沒收好之類的格式問題：csv 給得出行號，所以照樣記成一行被剔掉的紀錄。
			// 不是 ParseError 的話代表底下的 io.Reader 出事了，那跟檔案內容無關，
			// 整份放棄比記成一行被剔掉的紀錄誠實。
			var pe *csv.ParseError
			if !errors.As(err, &pe) {
				return run, err
			}
			run.Rows++
			run.Rejected = append(run.Rejected, Reject{
				Line: pe.Line, Reason: ReasonFields, Detail: pe.Err.Error(),
			})
			continue
		}
		line, _ := cr.FieldPos(0)
		run.Rows++
		if rj, ok := check(rec, line, seen); !ok {
			run.Rejected = append(run.Rejected, rj)
			continue
		}
		id, merchant, amount := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1]), strings.TrimSpace(rec[2])
		run.Accepted = append(run.Accepted, bulk.Payout{
			Ref: paymentref.Derive(paymentref.Terms{
				IntentID: id,
				Chain:    t.Chain,
				Token:    t.Token,
				Payer:    t.Payer,
				Merchant: merchant,
				Amount:   amount,
			}),
			Merchant: merchant,
			Amount:   mustAmount(amount),
		})
		run.Lines = append(run.Lines, line)
	}

	if run.Rows == 0 {
		return run, ErrNoRows
	}
	if n := len(run.Rejected); n > 0 {
		switch {
		case !p.Skip:
			run.Accepted, run.Lines = nil, nil
			return run, fmt.Errorf("%w: %d of %d rows", ErrRejected, n, run.Rows)
		case p.MaxRejects > 0 && n > p.MaxRejects:
			run.Accepted, run.Lines = nil, nil
			return run, fmt.Errorf("%w: %d rejected, the limit is %d", ErrTooManyRejects, n, p.MaxRejects)
		}
	}
	return run, nil
}

func sameHeader(got []string) bool {
	if len(got) != len(header) {
		return false
	}
	for i := range header {
		if strings.TrimSpace(strings.ToLower(got[i])) != header[i] {
			return false
		}
	}
	return true
}

// check 逐欄驗一行，回第一個不合格的理由，順便把看過的 intent id 記進 seen。
// 前後空白一律先修掉：試算表很會加空白，那不是資料錯誤，但空白以外的東西一個字都不改。
func check(rec []string, line int, seen map[string]int) (Reject, bool) {
	if len(rec) != len(header) {
		return Reject{Line: line, Reason: ReasonFields,
			Detail: fmt.Sprintf("the row has %d fields, want %d", len(rec), len(header))}, false
	}
	id := strings.TrimSpace(rec[0])
	if !identifier(id) {
		return Reject{Line: line, Reason: ReasonIntentID,
			Detail: fmt.Sprintf("%q is not an intent id", rec[0])}, false
	}
	if before, ok := seen[id]; ok {
		return Reject{Line: line, Reason: ReasonDuplicate,
			Detail: fmt.Sprintf("%s already appears on line %d", id, before)}, false
	}
	// 記住這個 id 的時機刻意在這裡，不是等這一行整行過關才記：這一行後面幾欄壞掉被剔除之後，
	// 同一個 id 還是不准在這份檔案裡出現第二次。不然報告會叫人去修被剔掉的那一行，
	// 修好重送就變成同一筆 intent 付兩次。
	seen[id] = line
	if !identifier(strings.TrimSpace(rec[1])) {
		return Reject{Line: line, Reason: ReasonMerchant,
			Detail: fmt.Sprintf("%q is not a merchant", rec[1])}, false
	}
	if _, ok := minorUnits(strings.TrimSpace(rec[2])); !ok {
		return Reject{Line: line, Reason: ReasonAmount,
			Detail: fmt.Sprintf("%q is not a whole number of minor units", rec[2])}, false
	}
	return Reject{}, true
}

// identifier 是 intent id 與 merchant 共用的檢查：非空、不太長、沒有空白與逗號。
func identifier(s string) bool {
	if s == "" || len(s) > maxField {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n,\"")
}

// minorUnits 只收正整數的字串。小數點一律不收：「100.00」要換算成最小單位得先知道這顆 token
// 有幾位小數，而那是 token 的屬性，不是這一行講得出來的事。USDC 是 6 位，猜成 2 位就差一萬倍。
func minorUnits(s string) (*big.Int, bool) {
	if s == "" || len(s) > 40 {
		return nil, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() <= 0 {
		return nil, false
	}
	return n, true
}

// mustAmount 只在 check 已經放行之後被呼叫，所以這裡不可能失敗。
func mustAmount(s string) *big.Int {
	n, _ := minorUnits(s)
	return n
}
