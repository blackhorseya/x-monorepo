package payment

import (
	"context"
	"log/slog"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
)

// Expire is `payment-rpc expire`: one run of the sweep, the way a CronJob runs it. It lives here, not in
// the composition root, because it holds a use case, which apps/ may not touch.
type Expire struct {
	sweep  ExpireUC
	logger *slog.Logger
}

func NewExpire(sweep ExpireUC, logger *slog.Logger) *Expire {
	return &Expire{sweep: sweep, logger: logger}
}

// Run sweeps once and logs what it did (counts only). An error means the run could not finish its work
// (the list failed, or a payment could not be saved); provider trouble is NOT an error: the payments
// concerned did not move and come round again next run.
func (x *Expire) Run(c context.Context) error {
	x.logger.InfoContext(c, "payment-rpc expire started", buildinfo.Get().LogAttrs()...)
	out, err := x.sweep.Execute(c, dto.Ack{})
	if out != nil {
		x.logger.InfoContext(c, "payment-rpc expire finished",
			"expired", out.Expired, "captured", out.Captured, "needs_review", out.NeedsReview,
			"query_failures", out.QueryFailures, "conflicts", out.Conflicts, "failures", out.Failures,
			"retention_deleted", out.RetentionDeleted, "retention_failed", out.RetentionFailed)
	}

	return err
}
