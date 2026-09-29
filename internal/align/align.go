package align

import "strings"

type TimedToken struct {
	Text           string
	StartMS, EndMS int64
}
type CanonToken struct {
	Text           string
	StartMS, EndMS int64
	Confidence     float64
}

func norm(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// AlignTokens is intentionally small but real: dynamic-programming sequence alignment
// that transfers ASR timing to matching canonical ebook tokens. The production agent
// should extend normalization/contraction/fuzzy scoring and interpolation per docs.
func AlignTokens(book []string, asr []TimedToken) []CanonToken {
	n, m := len(book), len(asr)
	score := make([][]int, n+1)
	move := make([][]byte, n+1)
	for i := range score {
		score[i] = make([]int, m+1)
		move[i] = make([]byte, m+1)
	}
	for i := 1; i <= n; i++ {
		score[i][0] = score[i-1][0] - 2
		move[i][0] = 'U'
	}
	for j := 1; j <= m; j++ {
		score[0][j] = score[0][j-1] - 2
		move[0][j] = 'L'
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			match := -2
			if norm(book[i-1]) == norm(asr[j-1].Text) && norm(book[i-1]) != "" {
				match = 4
			}
			a := score[i-1][j-1] + match
			u := score[i-1][j] - 2
			l := score[i][j-1] - 2
			score[i][j] = a
			move[i][j] = 'D'
			if u > score[i][j] {
				score[i][j] = u
				move[i][j] = 'U'
			}
			if l > score[i][j] {
				score[i][j] = l
				move[i][j] = 'L'
			}
		}
	}
	out := make([]CanonToken, n)
	for i, t := range book {
		out[i] = CanonToken{Text: t}
	}
	i, j := n, m
	for i > 0 || j > 0 {
		switch move[i][j] {
		case 'D':
			if norm(book[i-1]) == norm(asr[j-1].Text) && norm(book[i-1]) != "" {
				out[i-1].StartMS = asr[j-1].StartMS
				out[i-1].EndMS = asr[j-1].EndMS
				out[i-1].Confidence = 1
			}
			i--
			j--
		case 'U':
			i--
		case 'L':
			j--
		default:
			if i > 0 {
				i--
			} else if j > 0 {
				j--
			}
		}
	}
	return out
}
