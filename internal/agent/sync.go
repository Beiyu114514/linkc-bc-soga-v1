package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/node"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// Sync fetches /node and /users, then applies the smallest safe change.
func (a *Agent) Sync(ctx context.Context) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	a.mu.Lock()
	engine := a.engine
	hasUsers := a.hasUsers
	curSpec := a.nodeSpec
	curUsers := append([]xray.User(nil), a.users...)
	curUsersFP := a.usersFP
	curXC := a.xrayCfg
	curInboundFP := a.inboundFP
	auditLoaded := a.auditLoaded
	auditPending := a.auditPending
	a.mu.Unlock()
	if engine == nil {
		return fmt.Errorf("agent is not bootstrapped")
	}
	a.flushPendingRemove(ctx, engine)

	var rawNode *panel.Node
	var rawUsers []panel.User
	var rawRules []panel.AuditRule
	var rawWhite []string
	usersFetched := false
	rulesChanged := false
	whiteChanged := false
	auditOK := false
	var nodeErr, usersErr, rulesErr error

	if n, changed, err := a.panel.FetchNode(ctx); err != nil {
		nodeErr = fmt.Errorf("fetch node: %w", err)
		a.recordFailure("node", err)
	} else {
		a.recordSuccess("node")
		// Always use the returned body. A 304 may come from a cache that is
		// newer than last_good (e.g. after a failed apply + restart).
		_ = changed
		rawNode = &n
	}
	if u, changed, err := a.panel.FetchUsers(ctx); err != nil {
		usersErr = fmt.Errorf("fetch users: %w", err)
		a.recordFailure("users", err)
	} else {
		a.recordSuccess("users")
		_ = changed
		rawUsers = u
		usersFetched = true
	}
	if a.audit != nil {
		rules, rChanged, rErr := a.panel.FetchAuditRules(ctx)
		if rErr != nil {
			rulesErr = fmt.Errorf("fetch audit_rules: %w", rErr)
			a.recordFailure("audit_rules", rErr)
		} else {
			a.recordSuccess("audit_rules")
			rawRules, rulesChanged = rules, rChanged
		}
		white, wChanged, wErr := a.panel.FetchWhiteList(ctx)
		if wErr != nil {
			rulesErr = errors.Join(rulesErr, fmt.Errorf("fetch white_list: %w", wErr))
			a.recordFailure("white_list", wErr)
		} else {
			a.recordSuccess("white_list")
			rawWhite, whiteChanged = white, wChanged
		}
		auditOK = rErr == nil && wErr == nil
		if !auditOK && (rulesChanged || whiteChanged) {
			// A newer version was fetched but cannot be applied because the
			// other endpoint failed. Remember it: the advanced ETag makes the
			// next pull answer 304, which would otherwise skip the update for
			// good. The cached body still fills rawRules/rawWhite on that pull.
			a.setAuditPending(true)
		}
	}

	spec := curSpec
	xc := curXC
	nodeUpdated := false
	if rawNode != nil {
		sp, err := node.FromPanel(*rawNode)
		if err != nil {
			a.setApplyError("node", err)
			nodeErr = errors.Join(nodeErr, fmt.Errorf("validate node: %w", err))
		} else {
			nxc, err := sp.Xray(a.localRuntime())
			if err != nil {
				a.setApplyError("node", err)
				nodeErr = errors.Join(nodeErr, fmt.Errorf("build xray config: %w", err))
			} else {
				spec, xc, nodeUpdated = sp, nxc, true
			}
		}
	}

	nextUsers := curUsers
	usersUpdated := false
	if usersFetched {
		us, stats, err := node.NormalizeUsers(rawUsers, a.userOptions(), curUsers)
		if err != nil {
			a.setApplyError("users", err)
			usersErr = errors.Join(usersErr, fmt.Errorf("normalize users: %w", err))
		} else {
			nextUsers, usersUpdated = us, true
			if stats.Invalid > 0 || stats.Duplicates > 0 || stats.MassRemove {
				a.log.Warn("user list normalized", "raw", stats.Raw, "accepted", stats.Accepted,
					"invalid", stats.Invalid, "duplicates", stats.Duplicates, "removed", stats.Removed, "mass_remove", stats.MassRemove)
			}
		}
	}

	nodeCfgChanged := nodeUpdated && node.InboundFingerprint(xc) != curInboundFP
	usersChanged := usersUpdated && (!hasUsers || node.UsersFingerprint(nextUsers) != curUsersFP)
	usersApplied := false
	var applyErr error

	if nodeCfgChanged {
		a.collect(ctx)
		var rep xray.SwapReport
		var err error
		if a.swapInboundFn != nil {
			rep, err = a.swapInboundFn(engine, ctx, xc, nextUsers)
		} else {
			rep, err = engine.SwapInbound(ctx, xc, nextUsers)
		}
		if rep.StaleTag != "" {
			a.log.Error("inbound swap left a stale listener; queued for retry", "tag", rep.StaleTag, "error", err)
			a.queueRemove(rep.StaleTag)
		}
		switch {
		case rep.Applied:
			engine.SetNodeSpeed(spec.Basic.SpeedLimit)
			a.commitNode(spec, xc, nextUsers)
			usersApplied = true
			if usersUpdated {
				a.setApplyError("users", nil)
			}
			nodeUpdated = false
			if err != nil {
				applyErr = errors.Join(applyErr, fmt.Errorf("swap inbound: %w", err))
				a.setApplyError("node", err)
				a.log.Warn("inbound swapped with warning", "warning", rep.Warning, "error", err)
			} else {
				a.setApplyError("node", nil)
				if rep.Warning != "" {
					a.log.Warn("inbound swap warning", "warning", rep.Warning)
				}
				a.log.Info("inbound applied", "port", spec.Config.Port, "tag", rep.Tag)
			}
		case rep.RolledBack:
			// The engine restored the previous inbound; keep agent state
			// unchanged and retry on the next pull.
			applyErr = errors.Join(applyErr, fmt.Errorf("swap inbound: %w", err))
			a.setApplyError("node", err)
			nodeUpdated = false
			a.log.Warn("inbound swap rolled back", "error", err)
		default:
			// Add failed or there is no listener: keep the old state and retry.
			applyErr = errors.Join(applyErr, fmt.Errorf("swap inbound: %w", err))
			a.setApplyError("node", err)
			nodeUpdated = false
			if rep.Tag == "" {
				a.mu.Lock()
				a.inboundFP = ""
				a.mu.Unlock()
			}
			a.log.Error("inbound swap failed; keeping previous config", "error", err)
		}
		a.collect(ctx)
	}

	if nodeUpdated {
		// Only scheduling/basic fields changed, or the inbound fingerprint is
		// unchanged. Persist the new spec without touching the listener.
		engine.SetNodeSpeed(spec.Basic.SpeedLimit)
		a.mu.Lock()
		a.nodeSpec = spec
		a.xrayCfg = xc
		a.hasNode = true
		a.updateIntervalsLocked(spec)
		a.mu.Unlock()
		a.saveNode(spec)
		a.setApplyError("node", nil)
	}

	if usersChanged && !usersApplied {
		a.collect(ctx)
		if err := engine.ApplyUsers(ctx, nextUsers); err != nil {
			applyErr = errors.Join(applyErr, fmt.Errorf("apply users: %w", err))
			a.setApplyError("users", err)
		} else {
			a.setApplyError("users", nil)
			a.mu.Lock()
			a.users, a.usersFP = nextUsers, node.UsersFingerprint(nextUsers)
			a.hasUsers = true
			a.mu.Unlock()
			a.saveUsers(nextUsers)
		}
		a.collect(ctx)
	} else if usersUpdated && !usersChanged {
		a.setApplyError("users", nil)
	}

	if a.audit != nil {
		a.audit.Prune()
	}
	if auditOK && a.audit != nil {
		// The local whitelist file may have changed on disk; reload it on every
		// pull so edits take effect even when the panel answers 304.
		localChanged := false
		var local []string
		if a.cfg.Node.Audit.WhiteListFile != "" {
			if l, err := loadWhiteList(a.cfg.Node.Audit.WhiteListFile); err != nil {
				a.log.Warn("reload whitelist", "error", err)
			} else if !equalStrings(a.localWhite, l) {
				local, localChanged = l, true
			}
		}
		if rulesChanged || whiteChanged || localChanged || !auditLoaded || auditPending {
			// Merge against the whitelist that will be live after this update,
			// not the one still in a.localWhite: the latter is only advanced on
			// success, and a stale merge would never be retried.
			effective := a.localWhite
			if localChanged {
				effective = local
			}
			merged := mergeWhiteList(rawWhite, effective)
			stats, err := a.audit.Update(rawRules, merged)
			if err != nil {
				rulesErr = errors.Join(rulesErr, fmt.Errorf("apply audit rules: %w", err))
				a.setApplyError("audit", err)
				// Retry on the next pull: the fetched version is not live yet.
				a.setAuditPending(true)
			} else {
				a.setApplyError("audit", nil)
				// Only advance the local whitelist once the new set is live, so it
				// never leads the applied rules.
				if localChanged {
					a.localWhite = local
				}
				// Persist the version that is actually live, not just the one that
				// changed in this pull: a retried update sees changed=false because
				// the ETag already advanced, and gating the write on changed would
				// leave last_good behind. A restart would then replay the older
				// set and the newer cached body would never be applied again.
				// auditOK guarantees both fetches succeeded, so rawRules/rawWhite
				// hold the applied version (fresh body or 304 cache).
				if err := a.store.PutLastGood(a.lastGood("audit_rules"), rawRules); err != nil {
					a.log.Warn("save last_good audit_rules", "error", err)
				}
				if err := a.store.PutLastGood(a.lastGood("white_list"), rawWhite); err != nil {
					a.log.Warn("save last_good white_list", "error", err)
				}
				a.mu.Lock()
				a.auditLoaded = true
				a.auditPending = false
				a.mu.Unlock()
				if stats.Compiled > 0 || stats.Skipped > 0 {
					a.log.Info("audit rules applied", "compiled", stats.Compiled, "skipped", stats.Skipped)
				}
			}
		}
	}

	return errors.Join(nodeErr, usersErr, rulesErr, applyErr)
}

func (a *Agent) specPanel(spec node.Spec) panel.Node {
	return panel.Node{Basic: spec.Basic, Config: spec.Config, NodeType: spec.NodeType, TrafficRate: spec.TrafficRate}
}

// equalStrings reports whether two string slices hold the same elements.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
