package classifier

import (
	"fmt"
	"testing"
)

func names(accts []AccountCandidate) []string {
	out := make([]string, len(accts))
	for i, a := range accts {
		out[i] = a.Name
	}
	return out
}

func TestSelectCandidatesRanksBySimilarity(t *testing.T) {
	accts := []AccountCandidate{
		{ID: "1", Name: "Amazon"},
		{ID: "2", Name: "Lidl"},
		{ID: "3", Name: "Mercadona Online"},
		{ID: "4", Name: "Mercadona"},
	}
	got := names(selectCandidates(accts, "MERCADONA 4432 VALENCIA", nil, 2))
	if len(got) != 2 || got[0] != "Mercadona" || got[1] != "Mercadona Online" {
		t.Errorf("got %v, want [Mercadona Mercadona Online]", got)
	}
}

func TestSelectCandidatesCapsAndPinsHistory(t *testing.T) {
	var accts []AccountCandidate
	for i := 0; i < 500; i++ {
		accts = append(accts, AccountCandidate{ID: fmt.Sprint(i), Name: fmt.Sprintf("Shop %d", i)})
	}
	accts = append(accts, AccountCandidate{ID: "pinned", Name: "Corner Grocer"})
	history := []HistoricalEntry{{DestinationAccountID: "pinned"}}

	got := selectCandidates(accts, "SHOP PAYMENT", history, maxDestinationCandidates)
	if len(got) != maxDestinationCandidates {
		t.Fatalf("len = %d, want %d", len(got), maxDestinationCandidates)
	}
	if got[0].ID != "pinned" {
		t.Errorf("history account not first: %v", got[0])
	}
}

func TestSelectCandidatesDropsBlankNames(t *testing.T) {
	accts := []AccountCandidate{{ID: "1", Name: "(no name)"}, {ID: "2", Name: ""}, {ID: "3", Name: "Netflix"}}
	got := names(selectCandidates(accts, "NETFLIX.COM", nil, 50))
	if len(got) != 1 || got[0] != "Netflix" {
		t.Errorf("got %v, want [Netflix]", got)
	}
}

func TestSelectCandidatesKeepsOrderWithoutSignal(t *testing.T) {
	accts := []AccountCandidate{{ID: "1", Name: "B"}, {ID: "2", Name: "A"}}
	got := names(selectCandidates(accts, "", nil, 50))
	if len(got) != 2 || got[0] != "B" || got[1] != "A" {
		t.Errorf("got %v, want original order", got)
	}
}
