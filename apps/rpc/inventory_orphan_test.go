package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
)

// docOrphanSQL is the ```sql block under the orphan-stock entry of docs/operations.md: the
// query an operator is told to run before the upgrade, not a copy of it.
func docOrphanSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	const marker = "**部署前查孤兒庫存"
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("docs/operations.md has no %q section", marker)
	}
	m := regexp.MustCompile("(?s)```sql\n(.*?)```").FindStringSubmatch(doc[i:])
	if m == nil {
		t.Fatal("no ```sql block after the orphan stock marker")
	}

	return m[1]
}

// The runbook's query, run as written on the real schemas, lists exactly the stock that has no
// product — and stops listing it once the product exists.
func TestRunbook_OrphanStockQuery(t *testing.T) {
	s := newCatalogStockServer(t)
	c := context.Background()
	s.upsert(t, "HAS-PRODUCT")
	if err := s.setStock("HAS-PRODUCT", 2); err != nil {
		t.Fatal(err)
	}
	for _, sku := range []string{"ORPHAN-B", "ORPHAN-A"} {
		if err := s.db.WithContext(c).Exec(`INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES (?, 4, 1, 1, now())`, sku).Error; err != nil {
			t.Fatal(err)
		}
	}
	// a deactivated product is still a product: not an orphan
	s.upsert(t, "OFF-SALE")
	if err := s.setStock("OFF-SALE", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.catalog.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "OFF-SALE"})); err != nil {
		t.Fatal(err)
	}

	orphans := func() []string {
		t.Helper()
		var rows []struct {
			SKU      string
			OnHand   int32
			Reserved int32
		}
		if err := s.db.WithContext(c).Raw(docOrphanSQL(t)).Scan(&rows).Error; err != nil {
			t.Fatalf("the runbook's query failed: %v", err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.SKU)
		}

		return out
	}

	if got := orphans(); len(got) != 2 || got[0] != "ORPHAN-A" || got[1] != "ORPHAN-B" {
		t.Fatalf("orphans = %v, want [ORPHAN-A ORPHAN-B] in SKU order", got)
	}
	s.upsert(t, "ORPHAN-A")
	if got := orphans(); len(got) != 1 || got[0] != "ORPHAN-B" {
		t.Fatalf("orphans after UpsertProduct(ORPHAN-A) = %v, want [ORPHAN-B]", got)
	}
}
