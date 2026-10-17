package postgres

import (
	"context"

	"gorm.io/gorm"
)

type txKey struct{}

// WithTx returns a context carrying tx. Repositories and the outbox store pick
// it up through TxOrDB, so everything a use case does inside Transactor.Do
// lands in one transaction without the use case ever seeing *gorm.DB.
func WithTx(c context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(c, txKey{}, tx)
}

// TxOrDB returns the transaction carried by c, or db when there is none.
// The result already has c attached (WithContext).
func TxOrDB(c context.Context, db *gorm.DB) *gorm.DB {
	if tx, ok := c.Value(txKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(c)
	}

	return db.WithContext(c)
}

// Transaction runs fn inside the transaction carried by c, or opens a new one
// on db when there is none. A repository that needs atomicity across several
// statements uses this instead of db.Transaction: when the use case already
// opened a transaction the work joins it instead of nesting a SAVEPOINT.
func Transaction(c context.Context, db *gorm.DB, fn func(c context.Context, tx *gorm.DB) error) error {
	if tx, ok := c.Value(txKey{}).(*gorm.DB); ok && tx != nil {
		return fn(c, tx.WithContext(c))
	}

	return db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		return fn(WithTx(c, tx), tx)
	})
}
