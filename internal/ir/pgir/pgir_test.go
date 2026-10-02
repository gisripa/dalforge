package pgir

import "testing"

func TestLookupsFallBackToEmpty(t *testing.T) {
	h := &Hints{Tables: map[string]*Table{
		"orders.v1.Order": {IndexBudget: 4, Columns: map[int32]*Column{3: {Default: "'pending'"}}},
	}}

	order := h.Table("orders.v1.Order")
	if order.IndexBudget != 4 {
		t.Errorf("Table(Order).IndexBudget = %d, want 4", order.IndexBudget)
	}
	if got := order.Column(3).Default; got != "'pending'" {
		t.Errorf("Column(3).Default = %q, want 'pending'", got)
	}
	if got := order.Column(99); got == nil || *got != (Column{}) {
		t.Errorf("Column(99) = %v, want an empty Column", got)
	}
	if got := h.Table("orders.v1.Missing"); got == nil || got.Column(1) == nil {
		t.Error("Table(missing) must return a usable empty Table")
	}

	var empty Hints // zero value, as when an IDL sets no pg options
	if empty.Table("x").Column(1) == nil {
		t.Error("zero Hints must be usable")
	}
}
