package txpool

// DefaultPriceBumpPct 是同账户同 nonce 替换所需的最小费用提升百分比（10%）。
const DefaultPriceBumpPct uint64 = 10

// effectiveTip 计算给定 baseFee 下的实际优先费：
//
//	min(tipCap, feeCap-baseFee)
//
// feeCap 不足以覆盖 baseFee 时返回 0（交易无法被打包，但可暂存池中）。
func effectiveTip(tipCap, feeCap, baseFee uint64) uint64 {
	if feeCap <= baseFee {
		return 0
	}
	tip := feeCap - baseFee
	if tipCap < tip {
		tip = tipCap
	}
	return tip
}

// bumped 返回满足 bumpPct 提升后的门槛值：old*(100+bumpPct)/100，向上取整。
func bumped(old, bumpPct uint64) uint64 {
	num, ok := saturatingMul(old, 100+bumpPct)
	if !ok {
		return ^uint64(0)
	}
	r := num / 100
	if num%100 != 0 {
		r++ // 向上取整，确保严格超过 bumpPct
	}
	return r
}

// meetsReplacement 判断新报价是否同时在 feeCap 与有效小费两个维度上
// 相对旧报价满足 bumpPct 百分比的提升（两者都必须达标，防止
// "抬高 feeCap 却压低小费" 之类的取巧替换）。
func meetsReplacement(oldFeeCap, oldTipCap, newFeeCap, newTipCap, baseFee, bumpPct uint64) bool {
	if newFeeCap < bumped(oldFeeCap, bumpPct) {
		return false
	}
	oldEff := effectiveTip(oldTipCap, oldFeeCap, baseFee)
	newEff := effectiveTip(newTipCap, newFeeCap, baseFee)
	if newEff < bumped(oldEff, bumpPct) {
		return false
	}
	return true
}

// compareTx 按费用优先级比较两笔交易：
//  1. 有效小费（降序）；
//  2. 到达序号（升序，先到先得，保证确定性）；
//  3. from 地址（升序）；
//  4. nonce（升序）。
//
// 返回 <0 表示 a 优先于 b，>0 表示 b 优先，0 表示两者等价（实际不会出现）。
func compareTx(a, b *Transaction, baseFee uint64) int {
	at := effectiveTip(a.GasTipCap, a.GasFeeCap, baseFee)
	bt := effectiveTip(b.GasTipCap, b.GasFeeCap, baseFee)
	if at != bt {
		if at > bt {
			return -1
		}
		return 1
	}
	if a.seq != b.seq {
		if a.seq < b.seq {
			return -1
		}
		return 1
	}
	if c := compareAddress(a.From, b.From); c != 0 {
		return c
	}
	if a.Nonce != b.Nonce {
		if a.Nonce < b.Nonce {
			return -1
		}
		return 1
	}
	return 0
}

func compareAddress(a, b Address) int {
	for i := 0; i < 20; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
