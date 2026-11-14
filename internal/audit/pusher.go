package audit

import (
	"context"
	"fmt"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
)

// Push sends pending audit events. Events are removed only after code=0.
// The target site dedupes by (user, rule, node, natural day), so a duplicate
// POST is harmless.
func (c *Checker) Push(ctx context.Context, client *panel.Client, batch int) (int, error) {
	if c.st == nil {
		return 0, nil
	}
	if batch <= 0 {
		batch = 500
	}
	events, err := c.st.PendingAudit(batch)
	if err != nil {
		return 0, fmt.Errorf("audit: read queue: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}
	items := make([]panel.AuditLog, 0, len(events))
	seqs := make([]uint64, 0, len(events))
	for _, e := range events {
		items = append(items, panel.AuditLog{UserID: e.UserID, AuditID: e.RuleID})
		seqs = append(seqs, e.Seq)
	}
	if err := client.PushAuditLog(ctx, items); err != nil {
		return 0, err
	}
	if err := c.st.AckAudit(seqs); err != nil {
		return 0, fmt.Errorf("audit: ack queue: %w", err)
	}
	return len(items), nil
}
