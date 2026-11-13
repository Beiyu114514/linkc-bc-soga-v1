package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	a.mu.Unlock()
	if engine == nil {
		return fmt.Errorf("agent is not bootstrapped")
	}

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
	}

	spec := curSpec
	xc := curXC
	nodeUpdated := false
	if rawNode != nil {
		sp, err := node.FromPanel(*rawNode)
		if err != nil {
			a.setApplyError(err)
			nodeErr = errors.Join(nodeErr, fmt.Errorf("validate node: %w", err))
		} else {
			nxc, err := sp.Xray(a.localRuntime())
			if err != nil {
				a.setApplyError(err)
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
			a.setApplyError(err)
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
		tag := fmt.Sprintf("vless-%d", time.Now().UnixNano())
		oldTag := engine.Tag()
		if err := engine.AddInbound(ctx, xc, nextUsers, tag); err != nil {
			applyErr = errors.Join(applyErr, fmt.Errorf("add inbound: %w", err))
			a.setApplyError(err)
			nodeUpdated = false // keep the current spec; retry next pull
		} else {
			if err := engine.RemoveInbound(ctx, oldTag); err != nil {
				// The new listener is live; a stale listener is noisy but not fatal.
				a.log.Warn("remove old inbound", "tag", oldTag, "error", err)
			}
			engine.SetNodeSpeed(spec.Basic.SpeedLimit)
			a.mu.Lock()
			a.nodeSpec, a.xrayCfg = spec, xc
			a.inboundFP = node.InboundFingerprint(xc)
			a.users, a.usersFP = nextUsers, node.UsersFingerprint(nextUsers)
			a.hasNode, a.hasUsers = true, true
			a.updateIntervalsLocked(spec)
			a.mu.Unlock()
			a.saveNode(spec)
			a.saveUsers(nextUsers)
			usersApplied = true
			nodeUpdated = false
			a.log.Info("inbound applied", "port", spec.Config.Port, "tag", tag)
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
	}

	if usersChanged && !usersApplied {
		a.collect(ctx)
		if err := engine.ApplyUsers(ctx, nextUsers); err != nil {
			applyErr = errors.Join(applyErr, fmt.Errorf("apply users: %w", err))
			a.setApplyError(err)
		} else {
			a.mu.Lock()
			a.users, a.usersFP = nextUsers, node.UsersFingerprint(nextUsers)
			a.hasUsers = true
			a.mu.Unlock()
			a.saveUsers(nextUsers)
		}
		a.collect(ctx)
	}

	if auditOK && (rulesChanged || whiteChanged || !auditLoaded) && a.audit != nil {
		if rulesChanged {
			if err := a.store.PutLastGood("audit_rules", rawRules); err != nil {
				a.log.Warn("save last_good audit_rules", "error", err)
			}
		}
		if whiteChanged {
			if err := a.store.PutLastGood("white_list", rawWhite); err != nil {
				a.log.Warn("save last_good white_list", "error", err)
			}
		}
		// The local whitelist file may have changed; reload it each pull.
		if a.cfg.Node.Audit.WhiteListFile != "" {
			if local, err := loadWhiteList(a.cfg.Node.Audit.WhiteListFile); err != nil {
				a.log.Warn("reload whitelist", "error", err)
			} else {
				a.localWhite = local
			}
		}
		merged := a.mergeWhiteList(rawWhite)
		stats, err := a.audit.Update(rawRules, merged)
		if err != nil {
			rulesErr = errors.Join(rulesErr, fmt.Errorf("apply audit rules: %w", err))
			a.setApplyError(err)
		} else {
			a.mu.Lock()
			a.auditLoaded = true
			a.mu.Unlock()
			if stats.Compiled > 0 || stats.Skipped > 0 {
				a.log.Info("audit rules applied", "compiled", stats.Compiled, "skipped", stats.Skipped)
			}
		}
	}

	return errors.Join(nodeErr, usersErr, rulesErr, applyErr)
}

func (a *Agent) specPanel(spec node.Spec) panel.Node {
	return panel.Node{Basic: spec.Basic, Config: spec.Config, NodeType: spec.NodeType, TrafficRate: spec.TrafficRate}
}
