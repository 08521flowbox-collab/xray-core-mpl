package router

import (
	"net/netip"
	"sync"

	"github.com/xtls/xray-core/common/errors"
)

// Prebuilt conditions let an embedding application hand the router a matcher
// it built itself, keyed by rule tag, instead of describing the rule's domains
// or prefixes in the protobuf config. A geosite entry expressed as tens of
// thousands of Domain messages, cloned once per rule that needs it, costs more
// memory to carry than the finished matcher does; building once and sharing
// the result across rules keeps a memory-capped process alive.
var prebuilt = struct {
	sync.Mutex
	conditions map[string]Condition
}{conditions: map[string]Condition{}}

// SetPrebuiltCondition registers a condition for the rule with this tag. It is
// ANDed with whatever else the rule declares, exactly as a domain list would be.
func SetPrebuiltCondition(ruleTag string, c Condition) {
	prebuilt.Lock()
	defer prebuilt.Unlock()
	prebuilt.conditions[ruleTag] = c
}

// ResetPrebuiltConditions drops every registration; call it before building a
// new rule set so a tag from an earlier one cannot leak into this one.
func ResetPrebuiltConditions() {
	prebuilt.Lock()
	defer prebuilt.Unlock()
	prebuilt.conditions = map[string]Condition{}
}

func prebuiltCondition(ruleTag string) Condition {
	if ruleTag == "" {
		return nil
	}
	prebuilt.Lock()
	defer prebuilt.Unlock()
	return prebuilt.conditions[ruleTag]
}

// NewDomainCondition builds the same matcher a rule's Domain list would, from
// lists that never pass through protobuf: suffix rules and full names go into a
// succinct set, the rest into the MPH group.
func NewDomainCondition(full, suffix []string, others []*Domain) *DomainMatcher {
	return buildDomainMatcher(full, suffix, others)
}

// NewPrefixIPMatcher builds a destination-address condition from prefixes.
// Anything in include matches; anything not in exclude matches too, which is
// how a geoip:!xx entry is read. Either list may be empty, not both.
func NewPrefixIPMatcher(include, exclude []netip.Prefix, asType MatcherAsType) (*IPMatcher, error) {
	var subs []GeoIPMatcher
	if len(include) > 0 {
		subs = append(subs, NewPrefixSetMatcher(include, false))
	}
	if len(exclude) > 0 {
		subs = append(subs, NewPrefixSetMatcher(exclude, true))
	}
	switch len(subs) {
	case 0:
		return nil, errors.New("no prefixes for ip matcher")
	case 1:
		return &IPMatcher{matcher: subs[0], asType: asType}, nil
	default:
		return &IPMatcher{matcher: &anyGeoIPMatcher{subs: subs}, asType: asType}, nil
	}
}
