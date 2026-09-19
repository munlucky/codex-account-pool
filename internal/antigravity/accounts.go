package antigravity

import (
	"context"
	"github.com/munlucky/codex-account-pool/internal/accountpool"
	"github.com/munlucky/codex-account-pool/internal/antigravityauth"
)

// Registry access and credentials stay provider-specific; the policy sees IDs only.
type accountCredentialProvider interface {
	Accounts(context.Context) (string, []string, error)
	CredentialsFor(context.Context, string) (antigravityauth.Credentials, error)
}

type accountLease struct {
	policy   *accountpool.Lease
	broker   CredentialProvider
	ids      []string
	fallback map[string]antigravityauth.Credentials
	history  bool
}

func (c *Client) acquireAccount(ctx context.Context, session, model, selector string, history bool) (*accountLease, antigravityauth.Credentials, error) {
	policy, err := c.Pool.Acquire(ctx, "google-antigravity", session)
	if err != nil {
		return nil, antigravityauth.Credentials{}, err
	}
	if profile, ok := c.replay().SessionProfile(model, session); ok {
		if err := policy.RestoreProfile(profile); err != nil {
			policy.Release()
			return nil, antigravityauth.Credentials{}, err
		}
	}
	l := &accountLease{policy: policy, broker: c.Broker, history: history}
	policy.SetModel(model)
	preferred := ""
	if source, ok := c.Broker.(accountCredentialProvider); ok {
		preferred, l.ids, err = source.Accounts(ctx)
	} else {
		// Compatibility for single-account embedders. Production brokers load
		// only the selected snapshot, not every account's credentials.
		var current antigravityauth.Credentials
		current, err = c.Broker.Credentials(ctx)
		if err == nil {
			preferred = current.ProfileID
			l.ids = []string{preferred}
			l.fallback = map[string]antigravityauth.Credentials{preferred: current}
			if source, ok := c.Broker.(failoverCredentialProvider); ok {
				for _, candidate := range source.CandidateCredentials(ctx, preferred) {
					l.ids = append(l.ids, candidate.ProfileID)
					l.fallback[candidate.ProfileID] = candidate
				}
			}
		}
	}
	if err != nil {
		policy.Release()
		return nil, antigravityauth.Credentials{}, err
	}
	id, err := policy.Select(preferred, selector, l.ids, history)
	if err != nil {
		policy.Release()
		return nil, antigravityauth.Credentials{}, err
	}
	credentials, err := l.credentials(ctx, id)
	if err != nil {
		policy.Release()
		return nil, antigravityauth.Credentials{}, err
	}
	return l, credentials, nil
}

func (l *accountLease) credentials(ctx context.Context, id string) (antigravityauth.Credentials, error) {
	if source, ok := l.broker.(accountCredentialProvider); ok {
		return source.CredentialsFor(ctx, id)
	}
	if c, ok := l.fallback[id]; ok {
		return c, nil
	}
	return antigravityauth.Credentials{}, accountpool.ErrUnavailable
}

func (l *accountLease) failover(ctx context.Context, current antigravityauth.Credentials, model string, probe func(context.Context, antigravityauth.Credentials, string) quotaState) (antigravityauth.Credentials, bool) {
	if l.history || !l.policy.CanProbeFailover() {
		return antigravityauth.Credentials{}, false
	}
	probeCtx, cancel := quotaProbeContext(ctx)
	defer cancel() // One deadline bounds all probes and candidate refreshes.
	currentQuota := probe(probeCtx, current, model)
	l.policy.ObserveQuota(current.ProfileID, accountpool.Quota(currentQuota))
	var chosen antigravityauth.Credentials
	id := l.policy.Alternate(accountpool.Quota(currentQuota), l.ids, l.history, func(id string) accountpool.Quota {
		if probeCtx.Err() != nil {
			return accountpool.Unknown
		}
		credentials, err := l.credentials(probeCtx, id)
		if err != nil {
			return accountpool.Unknown
		}
		state := probe(probeCtx, credentials, model)
		l.policy.ObserveQuota(id, accountpool.Quota(state))
		if state == quotaUsable {
			chosen = credentials
		}
		return accountpool.Quota(state)
	})
	return chosen, id != ""
}
