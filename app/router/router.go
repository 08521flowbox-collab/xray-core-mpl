package router

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	routing_dns "github.com/xtls/xray-core/features/routing/dns"
)

// snapshot is everything PickRoute reads. It is built whole, published with a
// single atomic store and never mutated afterwards, so a reader walking it sees
// either the rule set that was live when it started or the one that replaced
// it — never a half-built list.
type snapshot struct {
	rules          []*Rule
	balancers      map[string]*Balancer
	domainStrategy Config_DomainStrategy
}

// emptySnapshot answers reads that arrive before Init has published anything.
// Init always runs first on the registered construction path, so this is a
// guard against a nil dereference, not a supported state.
var emptySnapshot = &snapshot{balancers: map[string]*Balancer{}}

// Router is an implementation of routing.Router.
type Router struct {
	current atomic.Pointer[snapshot]
	dns     dns.Client

	ctx        context.Context
	ohm        outbound.Manager
	dispatcher routing.Dispatcher
	// mu serialises writers against each other. Readers never take it: see
	// MODIFICATIONS.md for why a read lock is the wrong tool on this path.
	mu sync.Mutex
}

func (r *Router) load() *snapshot {
	if s := r.current.Load(); s != nil {
		return s
	}
	return emptySnapshot
}

// Route is an implementation of routing.Route.
type Route struct {
	routing.Context
	outboundGroupTags []string
	outboundTag       string
	ruleTag           string
}

// Init initializes the Router.
func (r *Router) Init(ctx context.Context, config *Config, d dns.Client, ohm outbound.Manager, dispatcher routing.Dispatcher) error {
	r.dns = d
	r.ctx = ctx
	r.ohm = ohm
	r.dispatcher = dispatcher

	next := &snapshot{
		domainStrategy: config.DomainStrategy,
		balancers:      make(map[string]*Balancer, len(config.BalancingRule)),
		rules:          make([]*Rule, 0, len(config.Rule)),
	}

	for _, rule := range config.BalancingRule {
		balancer, err := rule.Build(ohm, dispatcher)
		if err != nil {
			return err
		}
		balancer.InjectContext(ctx)
		next.balancers[rule.Tag] = balancer
	}

	for _, rule := range config.Rule {
		cond, err := rule.BuildCondition()
		if err != nil {
			closeWebhooks(next.rules)
			return err
		}
		rr := &Rule{
			Condition: cond,
			Tag:       rule.GetTag(),
			RuleTag:   rule.GetRuleTag(),
		}
		if wh := rule.GetWebhook(); wh != nil {
			notifier, err := NewWebhookNotifier(wh)
			if err != nil {
				closeWebhooks(next.rules)
				return err
			}
			rr.Webhook = notifier
		}
		btag := rule.GetBalancingTag()
		if len(btag) > 0 {
			brule, found := next.balancers[btag]
			if !found {
				if rr.Webhook != nil {
					rr.Webhook.Close()
				}
				closeWebhooks(next.rules)
				return errors.New("balancer ", btag, " not found")
			}
			rr.Balancer = brule
		}
		next.rules = append(next.rules, rr)
	}

	r.current.Store(next)
	return nil
}

// PickRoute implements routing.Router.
func (r *Router) PickRoute(ctx routing.Context) (routing.Route, error) {
	originalCtx := ctx
	rule, ctx, err := r.pickRouteInternal(ctx)
	if err != nil {
		return nil, err
	}
	tag, err := rule.GetTag()
	if err != nil {
		return nil, err
	}
	if rule.Webhook != nil {
		rule.Webhook.Fire(originalCtx, tag)
	}
	return &Route{Context: ctx, outboundTag: tag, ruleTag: rule.RuleTag}, nil
}

// AddRule implements routing.Router.
func (r *Router) AddRule(config *serial.TypedMessage, shouldAppend bool) error {

	inst, err := config.GetInstance()
	if err != nil {
		return err
	}
	if c, ok := inst.(*Config); ok {
		return r.ReloadRules(c, shouldAppend)
	}
	return errors.New("AddRule: config type error")
}

// ReloadRules replaces (shouldAppend == false) or extends (true) the live rule
// set. Everything is built into an unpublished snapshot first, so a failure
// anywhere leaves the router running exactly the rules it was running before —
// including their webhooks, which is why the old ones are closed only after the
// new snapshot is published.
func (r *Router) ReloadRules(config *Config, shouldAppend bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.load()
	next := &snapshot{}
	if shouldAppend {
		// Copy rather than extend in place: current is published and may be
		// under a reader right now.
		next.domainStrategy = current.domainStrategy
		next.rules = make([]*Rule, len(current.rules), len(current.rules)+len(config.Rule))
		copy(next.rules, current.rules)
		next.balancers = make(map[string]*Balancer, len(current.balancers)+len(config.BalancingRule))
		for tag, balancer := range current.balancers {
			next.balancers[tag] = balancer
		}
	} else {
		// A full replacement replaces the domain strategy too. Upstream keeps
		// the one Init saw, which leaves it unreachable at runtime; we drive
		// this from the UI and need to turn IpIfNonMatch on and off with the
		// rules that require it.
		next.domainStrategy = config.DomainStrategy
		next.rules = make([]*Rule, 0, len(config.Rule))
		next.balancers = make(map[string]*Balancer, len(config.BalancingRule))
	}

	startIdx := len(next.rules)
	closeNewWebhooks := func() {
		closeWebhooks(next.rules[startIdx:])
	}

	for _, rule := range config.BalancingRule {
		_, found := next.balancers[rule.Tag]
		if found {
			return errors.New("duplicate balancer tag")
		}
		balancer, err := rule.Build(r.ohm, r.dispatcher)
		if err != nil {
			return err
		}
		balancer.InjectContext(r.ctx)
		next.balancers[rule.Tag] = balancer
	}

	for _, rule := range config.Rule {
		// Checked against next, not the live set: within one call the rules
		// already built are the ones a duplicate tag would collide with.
		if ruleExists(next.rules, rule.GetRuleTag()) {
			closeNewWebhooks()
			return errors.New("duplicate ruleTag ", rule.GetRuleTag())
		}
		cond, err := rule.BuildCondition()
		if err != nil {
			closeNewWebhooks()
			return err
		}
		rr := &Rule{
			Condition: cond,
			Tag:       rule.GetTag(),
			RuleTag:   rule.GetRuleTag(),
		}
		if wh := rule.GetWebhook(); wh != nil {
			notifier, err := NewWebhookNotifier(wh)
			if err != nil {
				closeNewWebhooks()
				return err
			}
			rr.Webhook = notifier
		}
		btag := rule.GetBalancingTag()
		if len(btag) > 0 {
			brule, found := next.balancers[btag]
			if !found {
				if rr.Webhook != nil {
					rr.Webhook.Close()
				}
				closeNewWebhooks()
				return errors.New("balancer ", btag, " not found")
			}
			rr.Balancer = brule
		}
		next.rules = append(next.rules, rr)
	}

	r.current.Store(next)

	if !shouldAppend {
		// The dropped rules are unreachable now. Readers that started before
		// the store still hold them, but a webhook Close only stops the
		// notifier's own worker — it does not invalidate the Rule.
		closeWebhooks(current.rules)
	}
	return nil
}

func (r *Router) RuleExists(tag string) bool {
	return ruleExists(r.load().rules, tag)
}

func ruleExists(rules []*Rule, tag string) bool {
	if tag != "" {
		for _, rule := range rules {
			if rule.RuleTag == tag {
				return true
			}
		}
	}
	return false
}

// RemoveRule implements routing.Router.
func (r *Router) RemoveRule(tag string) error {
	if tag == "" {
		return errors.New("empty tag name!")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.load()
	next := &snapshot{
		domainStrategy: current.domainStrategy,
		balancers:      current.balancers,
		rules:          make([]*Rule, 0, len(current.rules)),
	}
	removed := []*Rule{}
	for _, rule := range current.rules {
		if rule.RuleTag != tag {
			next.rules = append(next.rules, rule)
		} else {
			removed = append(removed, rule)
		}
	}
	r.current.Store(next)
	closeWebhooks(removed)
	return nil
}

// ListRule implements routing.Router
func (r *Router) ListRule() []routing.Route {
	rules := r.load().rules
	ruleList := make([]routing.Route, 0, len(rules))
	for _, rule := range rules {
		ruleList = append(ruleList, &Route{
			outboundTag: rule.Tag,
			ruleTag:     rule.RuleTag,
		})
	}
	return ruleList
}

func (r *Router) pickRouteInternal(ctx routing.Context) (*Rule, routing.Context, error) {
	// SkipDNSResolve is set from DNS module.
	// the DOH remote server maybe a domain name,
	// this prevents cycle resolving dead loop
	skipDNSResolve := ctx.GetSkipDNSResolve()

	// One load for the whole decision: both passes must see the same rules,
	// and the second one runs after a DNS lookup that a reload can easily
	// outlive.
	s := r.load()

	if s.domainStrategy == Config_IpOnDemand && !skipDNSResolve {
		ctx = routing_dns.ContextWithDNSClient(ctx, r.dns)
	}

	for i, rule := range s.rules {
		if rule.Apply(ctx) {
			// A first-pass match on the *last* rule is provisional under
			// IpIfNonMatch. This consumer's rule table ends in a catch-all that
			// matches every connection, and with it in place "no rule matched"
			// — the condition the second, resolving pass hangs from — can never
			// occur: an address rule sitting above the catch-all could never
			// fire against a domain target. Resolving here and re-walking the
			// earlier rules restores the semantics the strategy promises; the
			// catch-all keeps the connection only when the resolved address
			// still matches nothing above it. A table without a catch-all is
			// untouched — its unmatched connections still take the pass below.
			if i == len(s.rules)-1 && s.domainStrategy == Config_IpIfNonMatch &&
				len(ctx.GetTargetDomain()) > 0 && !skipDNSResolve {
				resolving := routing_dns.ContextWithDNSClient(ctx, r.dns)
				for _, earlier := range s.rules[:i] {
					if earlier.Apply(resolving) {
						return earlier, resolving, nil
					}
				}
			}
			return rule, ctx, nil
		}
	}

	if s.domainStrategy != Config_IpIfNonMatch || len(ctx.GetTargetDomain()) == 0 || skipDNSResolve {
		return nil, ctx, common.ErrNoClue
	}

	ctx = routing_dns.ContextWithDNSClient(ctx, r.dns)

	// Try applying rules again if we have IPs.
	for _, rule := range s.rules {
		if rule.Apply(ctx) {
			return rule, ctx, nil
		}
	}

	return nil, ctx, common.ErrNoClue
}

// Start implements common.Runnable.
func (r *Router) Start() error {
	return nil
}

// closeWebhooks closes the webhook notifiers of the given rules.
func closeWebhooks(rules []*Rule) {
	for _, rule := range rules {
		if rule.Webhook != nil {
			rule.Webhook.Close()
		}
	}
}

// Close implements common.Closable.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	closeWebhooks(r.load().rules)
	return nil
}

// Type implements common.HasType.
func (*Router) Type() interface{} {
	return routing.RouterType()
}

// GetOutboundGroupTags implements routing.Route.
func (r *Route) GetOutboundGroupTags() []string {
	return r.outboundGroupTags
}

// GetOutboundTag implements routing.Route.
func (r *Route) GetOutboundTag() string {
	return r.outboundTag
}

func (r *Route) GetRuleTag() string {
	return r.ruleTag
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		r := new(Router)
		if err := core.RequireFeatures(ctx, func(d dns.Client, ohm outbound.Manager, dispatcher routing.Dispatcher) error {
			return r.Init(ctx, config.(*Config), d, ohm, dispatcher)
		}); err != nil {
			return nil, err
		}
		return r, nil
	}))
}
