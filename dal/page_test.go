package dal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPageSize(t *testing.T) {
	for _, tt := range []struct{ req, want int32 }{{0, 50}, {-3, 50}, {20, 20}, {500, 500}, {501, 500}} {
		if got := PageSize(tt.req, 50, 500); got != tt.want {
			t.Errorf("PageSize(%d) = %d, want %d", tt.req, got, tt.want)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	when := time.Date(2026, 10, 1, 12, 30, 45, 123456000, time.UTC) // microseconds, as Postgres stores
	filters := []any{"acct-1", "paid"}
	tok, err := EncodeToken("OrderListByAccount:created_at DESC,id DESC", filters, when, "row-9")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q isn't unpadded base64url", tok)
	}

	var gotWhen time.Time
	var gotID string
	if err := DecodeToken(tok, "OrderListByAccount:created_at DESC,id DESC", filters, &gotWhen, &gotID); err != nil {
		t.Fatal(err)
	}
	if !gotWhen.Equal(when) || gotID != "row-9" {
		t.Errorf("decoded %v, %q; want %v, row-9", gotWhen, gotID, when)
	}
}

func TestTokenRejects(t *testing.T) {
	shape, filters := "ListA", []any{"acct-1"}
	tok, _ := EncodeToken(shape, filters, 7)
	var k int
	tests := []struct {
		name    string
		token   string
		shape   string
		filters []any
		keys    []any
		want    string
	}{
		{name: "garbage", token: "%%%", shape: shape, filters: filters, keys: []any{&k}, want: "not base64url"},
		{name: "not json", token: "bm90LWpzb24", shape: shape, filters: filters, keys: []any{&k}, want: "malformed"},
		{name: "other list", token: tok, shape: "ListB", filters: filters, keys: []any{&k}, want: "different list"},
		{name: "other filters", token: tok, shape: shape, filters: []any{"acct-2"}, keys: []any{&k}, want: "different filters"},
		{name: "key count", token: tok, shape: shape, filters: filters, keys: []any{&k, &k}, want: "wrong number of keys"},
		{name: "key type", token: tok, shape: shape, filters: filters, keys: []any{new(time.Time)}, want: "key 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeToken(tt.token, tt.shape, tt.filters, tt.keys...)
			if !errors.Is(err, ErrInvalidPageToken) || !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("DecodeToken() = %v, want ErrInvalidPageToken containing %q", err, tt.want)
			}
		})
	}
}

// listParams is what generated params look like to All.
type listParams struct{ token string }

func (p listParams) WithPageToken(t string) listParams { p.token = t; return p }

func TestAll(t *testing.T) {
	pages := map[string]Page[int]{
		"":   {Items: []int{1, 2}, NextPageToken: "p2"},
		"p2": {Items: []int{3, 4}, NextPageToken: "p3"},
		"p3": {Items: []int{5}},
	}
	calls := 0
	list := func(_ context.Context, p listParams) (Page[int], error) {
		calls++
		return pages[p.token], nil
	}

	var got []int
	for v, err := range All(context.Background(), listParams{}, list) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if len(got) != 5 || got[4] != 5 || calls != 3 {
		t.Errorf("All = %v after %d calls, want [1 2 3 4 5] after 3", got, calls)
	}

	// Breaking early stops fetching.
	calls = 0
	for v := range All(context.Background(), listParams{}, list) {
		if v == 2 {
			break
		}
	}
	if calls != 1 {
		t.Errorf("break after 2 items fetched %d pages, want 1", calls)
	}

	// An error ends the iteration after yielding it.
	boom := errors.New("boom")
	var seen error
	for _, err := range All(context.Background(), listParams{}, func(context.Context, listParams) (Page[int], error) { return Page[int]{}, boom }) {
		seen = err
	}
	if !errors.Is(seen, boom) {
		t.Errorf("error = %v, want boom", seen)
	}
}

func TestNewPage(t *testing.T) {
	tok := func(last int) (string, error) { return "after-" + string(rune('0'+last)), nil }
	p, err := NewPage([]int{1, 2, 3}, 2, tok) // size+1 rows: another page exists
	if err != nil || len(p.Items) != 2 || p.NextPageToken != "after-2" {
		t.Errorf("full page = %+v, %v", p, err)
	}
	p, err = NewPage([]int{1, 2}, 2, tok) // exactly size: last page
	if err != nil || len(p.Items) != 2 || p.NextPageToken != "" {
		t.Errorf("last page = %+v, %v", p, err)
	}
	p, err = NewPage([]int{}, 2, tok)
	if err != nil || p.NextPageToken != "" {
		t.Errorf("empty page = %+v, %v; must not carry a token", p, err)
	}
}
