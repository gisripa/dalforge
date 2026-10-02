package dal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
)

// ErrInvalidPageToken means a page token couldn't be decoded, or belongs to
// a different list or different filters than the call it was passed to. It
// wraps ErrInvalidArgument, so it is never retried.
var ErrInvalidPageToken = fmt.Errorf("%w: invalid page token", ErrInvalidArgument)

// Page is one page of a keyset-paginated list. An empty NextPageToken means
// this is the last page; a non-empty page token is never returned for an
// empty page.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// PageSize clamps a requested page size: zero or negative means def, and
// anything above limit is limit.
func PageSize(requested, def, limit int32) int32 {
	switch {
	case requested <= 0:
		return def
	case requested > limit:
		return limit
	}
	return requested
}

// tokenVersion is bumped if the envelope ever changes shape.
const tokenVersion = 1

// envelope is a page token before base64url encoding (design §6). Tokens are
// not signed or encrypted: they carry the last row's sort keys, values the
// client has already seen. The hashes bind a token to its query and filters,
// for correctness, not security.
type envelope struct {
	V int               `json:"v"`
	Q string            `json:"q"` // hash of the query shape
	F string            `json:"f"` // hash of the filter values
	K []json.RawMessage `json:"k"` // last row's sort keys, then primary key
}

// EncodeToken builds the token for the page after the row with the given
// keys. shape identifies the list (generated code passes the query name and
// sort); filters are the call's equality and range arguments.
func EncodeToken(shape string, filters []any, keys ...any) (string, error) {
	f, err := hashJSON(filters)
	if err != nil {
		return "", err
	}
	env := envelope{V: tokenVersion, Q: hashString(shape), F: f}
	for _, k := range keys {
		b, err := json.Marshal(k)
		if err != nil {
			return "", fmt.Errorf("dal: encode page token key: %w", err)
		}
		env.K = append(env.K, b)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeToken checks that token belongs to shape and filters and decodes its
// keys into the given pointers. Any mismatch is ErrInvalidPageToken.
func DecodeToken(token, shape string, filters []any, keys ...any) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("%w: not base64url", ErrInvalidPageToken)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%w: malformed", ErrInvalidPageToken)
	}
	f, err := hashJSON(filters)
	if err != nil {
		return err
	}
	switch {
	case env.V != tokenVersion:
		return fmt.Errorf("%w: version %d", ErrInvalidPageToken, env.V)
	case env.Q != hashString(shape):
		return fmt.Errorf("%w: it belongs to a different list", ErrInvalidPageToken)
	case env.F != f:
		return fmt.Errorf("%w: it was issued for different filters", ErrInvalidPageToken)
	case len(env.K) != len(keys):
		return fmt.Errorf("%w: wrong number of keys", ErrInvalidPageToken)
	}
	for i, k := range keys {
		if err := json.Unmarshal(env.K[i], k); err != nil {
			return fmt.Errorf("%w: key %d: %v", ErrInvalidPageToken, i, err)
		}
	}
	return nil
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func hashJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("dal: hash page token filters: %w", err)
	}
	return hashString(string(b)), nil
}

// Paged is a list's parameters: they can carry the token of the next page.
// Generated params types implement it.
type Paged[P any] interface {
	WithPageToken(token string) P
}

// All iterates over every item of a list, page by page, for in-process use.
// Pages remain the primitive at API boundaries; All is the loop around them.
// It stops at the first error, yielding it.
//
//	for o, err := range dal.All(ctx, params, repo.ListOrdersByAccount) {
//		if err != nil { return err }
//		…
//	}
func All[T any, P Paged[P]](ctx context.Context, p P, list func(context.Context, P) (Page[T], error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for {
			page, err := list(ctx, p)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}
			if page.NextPageToken == "" {
				return
			}
			if err := ctx.Err(); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			p = p.WithPageToken(page.NextPageToken)
		}
	}
}

// NewPage builds a page from rows fetched with a limit of size+1: the extra
// row only signals that another page exists. token encodes the page token
// from the last row actually returned.
func NewPage[T any](rows []T, size int32, token func(last T) (string, error)) (Page[T], error) {
	if int32(len(rows)) <= size {
		return Page[T]{Items: rows}, nil
	}
	items := rows[:size]
	next, err := token(items[len(items)-1])
	if err != nil {
		return Page[T]{}, err
	}
	return Page[T]{Items: items, NextPageToken: next}, nil
}
