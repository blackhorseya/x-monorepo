package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestTranslate(t *testing.T) {
	plain := errors.New("boom")
	tests := []struct {
		name   string
		in     error
		want   error
		domain bool // want is a domain error; everything else must stay untranslated
	}{
		{"nil stays nil", nil, nil, false},
		{"record not found → not found", fmt.Errorf("find order x: %w", gorm.ErrRecordNotFound), order.ErrNotFound, true},
		{"duplicated key → duplicate", fmt.Errorf("upsert: %w", gorm.ErrDuplicatedKey), order.ErrDuplicate, true},
		{"fk violation stays infrastructure", fmt.Errorf("insert: %w", gorm.ErrForeignKeyViolated), gorm.ErrForeignKeyViolated, false},
		{"context cancelled passes through", context.Canceled, context.Canceled, false},
		{"unknown stays wrapped", fmt.Errorf("x: %w", plain), plain, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translate(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if !tt.domain && (errors.Is(got, order.ErrNotFound) || errors.Is(got, order.ErrDuplicate)) {
				t.Fatalf("got %v: must not be translated to a domain error", got)
			}
		})
	}
}
