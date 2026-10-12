package job

import "testing"

func TestSetFinishedStoresSource(t *testing.T) {
	r := NewRegistry()
	j := r.Create("42", "", "Mercadona", "MERCADONA 123", nil)

	r.SetFinished(j.ID, "CLASSIFIED", "Groceries", "reason", "", "p", "r", "Mercadona", "MATCH", "jev")

	got, ok := r.Get(j.ID)
	if !ok {
		t.Fatal("job not found")
	}
	if got.Source != "jev" {
		t.Errorf("Source = %q, want %q", got.Source, "jev")
	}
	if got.Status != StatusFinished {
		t.Errorf("Status = %q, want %q", got.Status, StatusFinished)
	}
}
