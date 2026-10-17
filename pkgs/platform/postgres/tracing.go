package postgres

import (
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// tracing is a small gorm plugin: one span per statement, child of whatever
// span is in the statement's context (the RPC span). Hand-rolled instead of
// gorm.io/plugin/opentelemetry, which drags ClickHouse and MySQL drivers into
// every binary. Spans carry the SQL shape ($1 placeholders), never the values.
type tracing struct {
	tracer trace.Tracer
}

const spanKey = "platform/postgres:span"

func (p tracing) Name() string { return "platform/postgres:tracing" }

// registrar is what gorm's Before()/After() return (an unexported type).
type registrar interface {
	Register(name string, fn func(*gorm.DB)) error
}

func (p tracing) Initialize(db *gorm.DB) error {
	cb := db.Callback()
	hook := func(op string, before, after registrar) error {
		return errors.Join(
			before.Register(p.Name()+":before_"+op, p.before(op)),
			after.Register(p.Name()+":after_"+op, p.after),
		)
	}

	return errors.Join(
		hook("create", cb.Create().Before("gorm:create"), cb.Create().After("gorm:create")),
		hook("query", cb.Query().Before("gorm:query"), cb.Query().After("gorm:query")),
		hook("update", cb.Update().Before("gorm:update"), cb.Update().After("gorm:update")),
		hook("delete", cb.Delete().Before("gorm:delete"), cb.Delete().After("gorm:delete")),
		hook("row", cb.Row().Before("gorm:row"), cb.Row().After("gorm:row")),
		hook("raw", cb.Raw().Before("gorm:raw"), cb.Raw().After("gorm:raw")),
	)
}

func (p tracing) before(op string) func(*gorm.DB) {
	return func(db *gorm.DB) {
		c, span := p.tracer.Start(db.Statement.Context, "gorm."+op,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system.name", "postgresql"),
				attribute.String("db.operation.name", op),
			),
		)
		db.Statement.Context = c
		db.Set(spanKey, span)
	}
}

func (p tracing) after(db *gorm.DB) {
	v, ok := db.Get(spanKey)
	if !ok {
		return
	}
	span, ok := v.(trace.Span)
	if !ok {
		return
	}
	defer span.End()

	span.SetAttributes(
		attribute.String("db.query.text", db.Statement.SQL.String()),
		attribute.String("db.collection.name", db.Statement.Table),
	)
	if err := db.Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}
