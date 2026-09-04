package trafficpolicy

import (
	"fmt"
	"sort"
	"time"

	cncfcorev3 "github.com/cncf/xds/go/xds/core/v3"
	cncfmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	envoycorev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoyroutev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	rlqsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rate_limit_quota/v3"
	envoymatcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	envoytypev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"istio.io/istio/pkg/kube/krt"

	"github.com/kgateway-dev/kgateway/v2/api/v1alpha1/kgateway"
	"github.com/kgateway-dev/kgateway/v2/pkg/kgateway/extensions2/pluginutils"
	"github.com/kgateway-dev/kgateway/v2/pkg/kgateway/utils"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/filters"
	"github.com/kgateway-dev/kgateway/v2/pkg/utils/cmputils"
)

const (
	rateLimitQuotaFilterNamePrefix = "ratelimit/quota"

	defaultRateLimitQuotaReportingInterval = 5 * time.Second
	defaultRateLimitQuotaDenyStatus        = 429
	// How long Envoy keeps enforcing the last assignment after it expires
	// before falling back to the no-assignment behavior.
	rateLimitQuotaExpiredAssignmentReuse = 30 * time.Second
)

// rateLimitQuotaIR is the intermediate representation for a quota-based
// (RLQS) rate limit policy. bucketSettings is the fully translated
// RateLimitQuotaBucketSettings action; the matcher predicate that selects
// it is derived from the route match at translation time.
type rateLimitQuotaIR struct {
	provider       *TrafficPolicyGatewayExtensionIR
	bucketSettings *rlqsv3.RateLimitQuotaBucketSettings
}

var _ PolicySubIR = &rateLimitQuotaIR{}

func (r *rateLimitQuotaIR) Equals(other PolicySubIR) bool {
	otherQuota, ok := other.(*rateLimitQuotaIR)
	if !ok {
		return false
	}
	if r == nil && otherQuota == nil {
		return true
	}
	if r == nil || otherQuota == nil {
		return false
	}
	if !proto.Equal(r.bucketSettings, otherQuota.bucketSettings) {
		return false
	}
	return cmputils.CompareWithNils(r.provider, otherQuota.provider, func(a, b *TrafficPolicyGatewayExtensionIR) bool {
		return a.Equals(*b)
	})
}

func (r *rateLimitQuotaIR) Validate() error {
	if r == nil {
		return nil
	}
	if r.bucketSettings != nil {
		if err := r.bucketSettings.ValidateAll(); err != nil {
			return err
		}
	}
	if r.provider != nil {
		return r.provider.Validate()
	}
	return nil
}

func constructRateLimitQuota(
	krtctx krt.HandlerContext,
	in *kgateway.TrafficPolicy,
	fetchGatewayExtension FetchGatewayExtensionFunc,
	out *trafficPolicySpecIr,
) error {
	if in.Spec.RateLimit == nil || in.Spec.RateLimit.Quota == nil {
		return nil
	}
	quota := in.Spec.RateLimit.Quota

	gwExtIR, err := fetchGatewayExtension(krtctx, quota.ExtensionRef, in.GetNamespace())
	if err != nil {
		return fmt.Errorf("ratelimit quota: %w", err)
	}
	if gwExtIR.RateLimitQuota == nil {
		return pluginutils.ErrInvalidExtensionType(kgateway.GatewayExtensionTypeRateLimitQuota)
	}

	out.rateLimitQuota = &rateLimitQuotaIR{
		provider:       gwExtIR,
		bucketSettings: buildRateLimitQuotaBucketSettings(quota),
	}
	return nil
}

func buildRateLimitQuotaBucketSettings(quota *kgateway.RateLimitQuotaPolicy) *rlqsv3.RateLimitQuotaBucketSettings {
	reportingInterval := defaultRateLimitQuotaReportingInterval
	if quota.ReportingInterval != nil {
		reportingInterval = quota.ReportingInterval.Duration
	}
	denyStatus := uint32(defaultRateLimitQuotaDenyStatus)
	if quota.DenyStatus != nil {
		denyStatus = *quota.DenyStatus
	}
	fallback := envoytypev3.RateLimitStrategy_ALLOW_ALL
	if quota.NoAssignmentBehavior != nil && *quota.NoAssignmentBehavior == kgateway.RateLimitQuotaFallbackDeny {
		fallback = envoytypev3.RateLimitStrategy_DENY_ALL
	}

	builder := make(map[string]*rlqsv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder, len(quota.Bucket))
	for k, v := range quota.Bucket {
		builder[k] = &rlqsv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder{
			ValueSpecifier: &rlqsv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder_StringValue{
				StringValue: v,
			},
		}
	}

	return &rlqsv3.RateLimitQuotaBucketSettings{
		BucketIdBuilder: &rlqsv3.RateLimitQuotaBucketSettings_BucketIdBuilder{
			BucketIdBuilder: builder,
		},
		ReportingInterval: durationpb.New(reportingInterval),
		DenyResponseSettings: &rlqsv3.RateLimitQuotaBucketSettings_DenyResponseSettings{
			HttpStatus: &envoytypev3.HttpStatus{Code: envoytypev3.StatusCode(denyStatus)},
		},
		NoAssignmentBehavior: &rlqsv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior{
			NoAssignmentBehavior: &rlqsv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior_FallbackRateLimit{
				FallbackRateLimit: &envoytypev3.RateLimitStrategy{
					Strategy: &envoytypev3.RateLimitStrategy_BlanketRule_{BlanketRule: fallback},
				},
			},
		},
		ExpiredAssignmentBehavior: &rlqsv3.RateLimitQuotaBucketSettings_ExpiredAssignmentBehavior{
			ExpiredAssignmentBehaviorTimeout: durationpb.New(rateLimitQuotaExpiredAssignmentReuse),
			ExpiredAssignmentBehavior: &rlqsv3.RateLimitQuotaBucketSettings_ExpiredAssignmentBehavior_ReuseLastAssignment_{
				ReuseLastAssignment: &rlqsv3.RateLimitQuotaBucketSettings_ExpiredAssignmentBehavior_ReuseLastAssignment{},
			},
		},
	}
}

// buildRateLimitQuotaFilter builds the HCM-level filter config from the
// GatewayExtension. bucket_matchers is populated per filter chain during
// translation from the routes the quota policies attach to.
func buildRateLimitQuotaFilter(grpcService *envoycorev3.GrpcService, provider *kgateway.RateLimitQuotaProvider) *rlqsv3.RateLimitQuotaFilterConfig {
	return &rlqsv3.RateLimitQuotaFilterConfig{
		RlqsServer: grpcService,
		Domain:     provider.Domain,
	}
}

// rateLimitQuotaChain accumulates, for one filter chain, the RLQS provider
// and the bucket matchers contributed by every quota policy attached to a
// route (or vhost/gateway, which becomes the on_no_match bucket) in it.
type rateLimitQuotaChain struct {
	provider  *TrafficPolicyGatewayExtensionIR
	matchers  []*cncfmatcherv3.Matcher_MatcherList_FieldMatcher
	onNoMatch *cncfmatcherv3.Matcher_OnMatch
}

// handleRateLimitQuota records a quota policy for the filter chain. When
// routeMatch is non-nil the bucket is selected by a predicate mirroring the
// route's path/header match; otherwise it becomes the chain's fallback bucket.
func (p *trafficPolicyPluginGwPass) handleRateLimitQuota(fcn string, quota *rateLimitQuotaIR, routeMatch *envoyroutev3.RouteMatch) {
	if quota == nil || quota.bucketSettings == nil || quota.provider == nil {
		return
	}
	if p.rateLimitQuotaInChain == nil {
		p.rateLimitQuotaInChain = make(map[string]*rateLimitQuotaChain)
	}
	chain := p.rateLimitQuotaInChain[fcn]
	if chain == nil {
		chain = &rateLimitQuotaChain{provider: quota.provider}
		p.rateLimitQuotaInChain[fcn] = chain
	}
	if chain.provider.ResourceName() != quota.provider.ResourceName() {
		// Envoy supports a single RLQS filter instance per chain in this MVP;
		// the first provider seen wins.
		logger.Warn("multiple RateLimitQuota extensions on one listener are not supported; ignoring",
			"filterChain", fcn, "used", chain.provider.ResourceName(), "ignored", quota.provider.ResourceName())
		return
	}

	onMatch := &cncfmatcherv3.Matcher_OnMatch{
		OnMatch: &cncfmatcherv3.Matcher_OnMatch_Action{
			Action: &cncfcorev3.TypedExtensionConfig{
				Name:        "rate_limit_quota",
				TypedConfig: utils.MustMessageToAny(quota.bucketSettings),
			},
		},
	}

	predicate := routeMatchPredicate(routeMatch)
	if predicate == nil {
		chain.onNoMatch = onMatch
		return
	}
	for _, m := range chain.matchers {
		if proto.Equal(m.Predicate, predicate) && proto.Equal(m.OnMatch, onMatch) {
			return
		}
	}
	chain.matchers = append(chain.matchers, &cncfmatcherv3.Matcher_MatcherList_FieldMatcher{
		Predicate: predicate,
		OnMatch:   onMatch,
	})
}

// routeMatchPredicate converts an Envoy RouteMatch into a unified matcher
// predicate on the request headers (:path plus any header matchers). Returns
// nil when the match is unconstrained (e.g. prefix "/" with no headers).
func routeMatchPredicate(rm *envoyroutev3.RouteMatch) *cncfmatcherv3.Matcher_MatcherList_Predicate {
	if rm == nil {
		return nil
	}
	var preds []*cncfmatcherv3.Matcher_MatcherList_Predicate

	var pathMatcher *cncfmatcherv3.StringMatcher
	switch ps := rm.GetPathSpecifier().(type) {
	case *envoyroutev3.RouteMatch_Path:
		pathMatcher = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_Exact{Exact: ps.Path}}
	case *envoyroutev3.RouteMatch_Prefix:
		if ps.Prefix != "" && ps.Prefix != "/" {
			pathMatcher = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_Prefix{Prefix: ps.Prefix}}
		}
	case *envoyroutev3.RouteMatch_PathSeparatedPrefix:
		pathMatcher = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_Prefix{Prefix: ps.PathSeparatedPrefix}}
	case *envoyroutev3.RouteMatch_SafeRegex:
		pathMatcher = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_SafeRegex{
			SafeRegex: &cncfmatcherv3.RegexMatcher{
				Regex:      ps.SafeRegex.GetRegex(),
				EngineType: &cncfmatcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &cncfmatcherv3.RegexMatcher_GoogleRE2{}},
			},
		}}
	}
	if pathMatcher != nil {
		if rm.GetCaseSensitive() != nil && !rm.GetCaseSensitive().GetValue() {
			pathMatcher.IgnoreCase = true
		}
		preds = append(preds, headerPredicate(":path", pathMatcher))
	}

	for _, h := range rm.GetHeaders() {
		var sm *cncfmatcherv3.StringMatcher
		switch hs := h.GetHeaderMatchSpecifier().(type) {
		case *envoyroutev3.HeaderMatcher_StringMatch:
			sm = envoyStringMatcherToXDS(hs.StringMatch)
		case *envoyroutev3.HeaderMatcher_ExactMatch:
			sm = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_Exact{Exact: hs.ExactMatch}}
		case *envoyroutev3.HeaderMatcher_PrefixMatch:
			sm = &cncfmatcherv3.StringMatcher{MatchPattern: &cncfmatcherv3.StringMatcher_Prefix{Prefix: hs.PrefixMatch}}
		}
		if sm == nil || h.GetInvertMatch() {
			continue
		}
		preds = append(preds, headerPredicate(h.GetName(), sm))
	}

	switch len(preds) {
	case 0:
		return nil
	case 1:
		return preds[0]
	default:
		return &cncfmatcherv3.Matcher_MatcherList_Predicate{
			MatchType: &cncfmatcherv3.Matcher_MatcherList_Predicate_AndMatcher{
				AndMatcher: &cncfmatcherv3.Matcher_MatcherList_Predicate_PredicateList{Predicate: preds},
			},
		}
	}
}

func envoyStringMatcherToXDS(in *envoymatcherv3.StringMatcher) *cncfmatcherv3.StringMatcher {
	if in == nil {
		return nil
	}
	out := &cncfmatcherv3.StringMatcher{IgnoreCase: in.GetIgnoreCase()}
	switch mp := in.GetMatchPattern().(type) {
	case *envoymatcherv3.StringMatcher_Exact:
		out.MatchPattern = &cncfmatcherv3.StringMatcher_Exact{Exact: mp.Exact}
	case *envoymatcherv3.StringMatcher_Prefix:
		out.MatchPattern = &cncfmatcherv3.StringMatcher_Prefix{Prefix: mp.Prefix}
	case *envoymatcherv3.StringMatcher_Suffix:
		out.MatchPattern = &cncfmatcherv3.StringMatcher_Suffix{Suffix: mp.Suffix}
	case *envoymatcherv3.StringMatcher_Contains:
		out.MatchPattern = &cncfmatcherv3.StringMatcher_Contains{Contains: mp.Contains}
	case *envoymatcherv3.StringMatcher_SafeRegex:
		out.MatchPattern = &cncfmatcherv3.StringMatcher_SafeRegex{SafeRegex: &cncfmatcherv3.RegexMatcher{
			Regex:      mp.SafeRegex.GetRegex(),
			EngineType: &cncfmatcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &cncfmatcherv3.RegexMatcher_GoogleRE2{}},
		}}
	default:
		return nil
	}
	return out
}

func headerPredicate(header string, sm *cncfmatcherv3.StringMatcher) *cncfmatcherv3.Matcher_MatcherList_Predicate {
	return &cncfmatcherv3.Matcher_MatcherList_Predicate{
		MatchType: &cncfmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
			SinglePredicate: &cncfmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
				Input: &cncfcorev3.TypedExtensionConfig{
					Name:        "request-header",
					TypedConfig: utils.MustMessageToAny(&envoymatcherv3.HttpRequestHeaderMatchInput{HeaderName: header}),
				},
				Matcher: &cncfmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: sm},
			},
		},
	}
}

// rateLimitQuotaHttpFilter builds the enabled HCM filter for a chain. Unlike
// the other TrafficPolicy filters it is not disabled and enabled per route:
// Envoy's rate_limit_quota filter has no per-route override, so route scoping
// is done by the bucket matcher tree instead. Requests matching no bucket
// are not rate limited.
func (p *trafficPolicyPluginGwPass) rateLimitQuotaHttpFilter(fcn string) *filters.StagedHttpFilter {
	chain := p.rateLimitQuotaInChain[fcn]
	if chain == nil || chain.provider.RateLimitQuota == nil {
		return nil
	}
	if len(chain.matchers) == 0 && chain.onNoMatch == nil {
		return nil
	}

	cfg := proto.Clone(chain.provider.RateLimitQuota).(*rlqsv3.RateLimitQuotaFilterConfig)
	matcher := &cncfmatcherv3.Matcher{OnNoMatch: chain.onNoMatch}
	if len(chain.matchers) > 0 {
		// Stable output: exact paths before prefixes so the most specific wins,
		// then lexical.
		matchers := append([]*cncfmatcherv3.Matcher_MatcherList_FieldMatcher(nil), chain.matchers...)
		sort.SliceStable(matchers, func(i, j int) bool {
			return matcherSortKey(matchers[i]) < matcherSortKey(matchers[j])
		})
		matcher.MatcherType = &cncfmatcherv3.Matcher_MatcherList_{
			MatcherList: &cncfmatcherv3.Matcher_MatcherList{Matchers: matchers},
		}
	}
	cfg.BucketMatchers = matcher

	f := filters.MustNewStagedFilter(rateLimitQuotaFilterNamePrefix, cfg, filters.DuringStage(filters.RateLimitStage))
	return &f
}

func matcherSortKey(m *cncfmatcherv3.Matcher_MatcherList_FieldMatcher) string {
	// AND predicates are more specific than single predicates; within a kind
	// exact sorts before prefix/regex.
	rank := "2"
	sp := m.GetPredicate().GetSinglePredicate()
	if m.GetPredicate().GetAndMatcher() != nil {
		rank = "0"
	} else if sp != nil && sp.GetValueMatch().GetExact() != "" {
		rank = "1"
	}
	return rank + m.GetPredicate().String()
}
