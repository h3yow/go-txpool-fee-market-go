package txpool

import (
	"fmt"
)

// RejectCode 是可区分的拒绝原因码。
type RejectCode string

const (
	// CodeInvalidFormat 字段非法：gas 为 0、tip>feeCap、成本溢出等。
	CodeInvalidFormat RejectCode = "invalid_format"
	// CodeNonceTooLow nonce 小于链上已执行 nonce（已过期）。
	CodeNonceTooLow RejectCode = "nonce_too_low"
	// CodeNonceGap nonce 与链上 nonce 之间存在空洞（超过允许前探距离）。
	CodeNonceGap RejectCode = "nonce_gap"
	// CodeInsufficientBalance 账户余额无法覆盖预付成本。
	CodeInsufficientBalance RejectCode = "insufficient_balance"
	// CodeReplaceUnderpriced 同账户同 nonce 替换但费用提升不足。
	CodeReplaceUnderpriced RejectCode = "replace_underpriced"
	// CodePoolFull 池满且新交易无法驱逐任何更低费交易。
	CodePoolFull RejectCode = "pool_full"
	// CodeAlreadyKnown 完全相同的交易已在池中。
	CodeAlreadyKnown RejectCode = "already_known"
	// CodeEvicted 交易因容量驱逐被移出（不用于 Submit 拒绝，供日志/事件使用）。
	CodeEvicted RejectCode = "evicted"
)

// 按原因码归类的哨兵错误，便于 errors.Is(err, ErrInsufficientBalance) 判定。
var (
	ErrInvalidFormat       = &RejectError{Code: CodeInvalidFormat, Reason: "transaction failed format validation"}
	ErrNonceTooLow         = &RejectError{Code: CodeNonceTooLow, Reason: "nonce is below the account's on-chain nonce"}
	ErrNonceGap            = &RejectError{Code: CodeNonceGap, Reason: "nonce leaves a gap from the account's on-chain nonce"}
	ErrInsufficientBalance = &RejectError{Code: CodeInsufficientBalance, Reason: "balance cannot cover upfront cost value + gasLimit*gasFeeCap"}
	ErrReplaceUnderpriced  = &RejectError{Code: CodeReplaceUnderpriced, Reason: "replacement transaction underpriced"}
	ErrPoolFull            = &RejectError{Code: CodePoolFull, Reason: "transaction pool is full and no lower-priced transaction is evictable"}
	ErrAlreadyKnown        = &RejectError{Code: CodeAlreadyKnown, Reason: "exact transaction already in pool"}
)

// RejectError 携带原因码、人类可读判定依据与交易哈希，
// 测试与日志可同时区分错误类别并还原判定输入。
type RejectError struct {
	Code   RejectCode
	Reason string
	TxHash [32]byte
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("tx %x rejected: %s (%s)", e.TxHash[:8], e.Reason, e.Code)
}

// Is 支持 errors.Is(err, ErrXxx) 按原因码归类；
// 带具体判定依据的错误仍能匹配对应哨兵。
func (e *RejectError) Is(target error) bool {
	t, ok := target.(*RejectError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// newReject 构造带具体判定依据的拒绝错误。
func newReject(code RejectCode, sentinel *RejectError, detail string, tx *Transaction) *RejectError {
	h := [32]byte{}
	if tx != nil {
		h = tx.Hash()
	}
	reason := sentinel.Reason
	if detail != "" {
		reason = reason + ": " + detail
	}
	return &RejectError{Code: code, Reason: reason, TxHash: h}
}

// formatDetail 统一构造判定依据文本（随错误返回并打印到日志）。
func formatDetail(format string, args ...any) string { return fmt.Sprintf(format, args...) }
