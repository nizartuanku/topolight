package backup

// Op is one line of a diff: ' ' same, '-' removed, '+' added.
type Op struct {
	Kind byte   `json:"k"`
	Line string `json:"l"`
	A    int    `json:"a,omitempty"` // 1-based line number in the old text
	B    int    `json:"b,omitempty"` // 1-based line number in the new text
}

// Diff computes a line diff (Myers, O(ND)) between a and b. Inputs of more
// than 20k lines fall back to a cheap prefix/suffix + block diff so a huge
// configuration never stalls the console.
func Diff(a, b []string) []Op {
	// common prefix / suffix
	n, m := len(a), len(b)
	p := 0
	for p < n && p < m && a[p] == b[p] {
		p++
	}
	s := 0
	for s < n-p && s < m-p && a[n-1-s] == b[m-1-s] {
		s++
	}
	var out []Op
	for i := 0; i < p; i++ {
		out = append(out, Op{' ', a[i], i + 1, i + 1})
	}
	ma, mb := a[p:n-s], b[p:m-s]
	if len(ma)+len(mb) > 40000 {
		for i, l := range ma {
			out = append(out, Op{'-', l, p + i + 1, 0})
		}
		for i, l := range mb {
			out = append(out, Op{'+', l, 0, p + i + 1})
		}
	} else {
		out = append(out, myers(ma, mb, p, p)...)
	}
	for i := 0; i < s; i++ {
		out = append(out, Op{' ', a[n-s+i], n - s + i + 1, m - s + i + 1})
	}
	return out
}

func myers(a, b []string, offA, offB int) []Op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	max := n + m
	v := make([]int, 2*max+2)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[max+k-1] < v[max+k+1]) {
				x = v[max+k+1]
			} else {
				x = v[max+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[max+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	// backtrack
	var ops []Op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		vv := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vv[max+k-1] < vv[max+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vv[max+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, Op{' ', a[x-1], offA + x, offB + y})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				ops = append(ops, Op{'+', b[y-1], 0, offB + y})
			} else {
				ops = append(ops, Op{'-', a[x-1], offA + x, 0})
			}
		}
		x, y = prevX, prevY
	}
	// reverse
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

// Counts returns added/removed line totals.
func Counts(ops []Op) (added, removed int) {
	for _, o := range ops {
		switch o.Kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return
}

// Hunks keeps only changed lines with ctx lines of context around them.
func Hunks(ops []Op, ctx int) []Op {
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.Kind != ' ' {
			for j := i - ctx; j <= i+ctx; j++ {
				if j >= 0 && j < len(ops) {
					keep[j] = true
				}
			}
		}
	}
	var out []Op
	last := -2
	for i, o := range ops {
		if !keep[i] {
			continue
		}
		if i != last+1 && len(out) > 0 {
			out = append(out, Op{Kind: '@'})
		}
		out = append(out, o)
		last = i
	}
	return out
}
