package classifier

import (
	"sort"
	"strings"
	"unicode"
)

// maxDestinationCandidates bounds how many expense accounts are offered to Jev
// so the request stays well inside its 32k-token context.
const maxDestinationCandidates = 50

// selectCandidates returns up to limit expense accounts to offer as destination
// options. Accounts already used for this payee in history come first; the rest
// are ranked by word overlap with groupKey, keeping the original order on ties.
func selectCandidates(accounts []AccountCandidate, groupKey string, history []HistoricalEntry, limit int) []AccountCandidate {
	pinned := make(map[string]bool)
	for _, h := range history {
		if h.DestinationAccountID != "" {
			pinned[h.DestinationAccountID] = true
		}
	}

	keyTokens := tokenize(groupKey)
	keyLower := strings.ToLower(strings.TrimSpace(groupKey))

	type scored struct {
		acct  AccountCandidate
		score float64
	}
	var first, rest []scored
	for _, a := range accounts {
		if isBlankName(a.Name) {
			continue
		}
		s := scored{acct: a, score: similarity(keyTokens, keyLower, a.Name)}
		if pinned[a.ID] {
			first = append(first, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].score > rest[j].score })

	out := make([]AccountCandidate, 0, limit)
	for _, s := range append(first, rest...) {
		if len(out) == limit {
			break
		}
		out = append(out, s.acct)
	}
	return out
}

// similarity is the share of the account name's words found in the key, plus a
// bonus when one string contains the other.
func similarity(keyTokens map[string]bool, keyLower, name string) float64 {
	nameTokens := tokenize(name)
	if len(nameTokens) == 0 || len(keyTokens) == 0 {
		return 0
	}
	shared := 0
	for t := range nameTokens {
		if keyTokens[t] {
			shared++
		}
	}
	score := float64(shared) / float64(len(nameTokens))
	nameLower := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(keyLower, nameLower) || strings.Contains(nameLower, keyLower) {
		score += 0.5
	}
	return score
}

func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[f] = true
	}
	return out
}
