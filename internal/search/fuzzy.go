package search

// withinEdit 判断两个字符串的编辑距离（Levenshtein）是否不超过 k。
func withinEdit(a, b string, k int) bool {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > k || -d > k {
		return false
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		minRow := cur[0]
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			v := prev[j-1] + c
			if x := prev[j] + 1; x < v {
				v = x
			}
			if x := cur[j-1] + 1; x < v {
				v = x
			}
			cur[j] = v
			if v < minRow {
				minRow = v
			}
		}
		if minRow > k {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)] <= k
}
