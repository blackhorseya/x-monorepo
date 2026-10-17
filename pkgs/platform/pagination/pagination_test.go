package pagination_test

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

func TestClamp(t *testing.T) {
	tests := []struct {
		size    int32
		want    int
		wantErr error
	}{
		{0, 20, nil},
		{5, 5, nil},
		{100, 100, nil},
		{101, 100, nil},
		{-1, 0, pagination.ErrInvalidPageSize},
	}
	for _, tt := range tests {
		got, err := pagination.Clamp(tt.size, 20, 100)
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Errorf("Clamp(%d) = %d, %v; want %d, %v", tt.size, got, err, tt.want, tt.wantErr)
		}
	}
}

type payload struct {
	Customer string `json:"c"`
	ID       string `json:"id"`
}

func TestToken_RoundTrip(t *testing.T) {
	in := payload{Customer: "c-1", ID: "o-1"}
	tok, err := pagination.EncodeToken(in)
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := pagination.DecodeToken(tok, &out); err != nil || out != in {
		t.Fatalf("decode = %+v, %v; want %+v", out, err, in)
	}
}

func TestToken_Invalid(t *testing.T) {
	for _, tok := range []string{"not base64!", "bm90IGpzb24", ""} {
		var out payload
		if err := pagination.DecodeToken(tok, &out); !errors.Is(err, pagination.ErrInvalidToken) {
			t.Errorf("DecodeToken(%q) err = %v, want ErrInvalidToken", tok, err)
		}
	}
}
