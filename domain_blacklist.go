package proxyd

import (
	"context"
	"net"
	"strings"

	"github.com/ethereum/go-ethereum/log"
)

func normalizeDomain(domain string) string {
	// X-Forwarded-Host can contain a proxy chain. The first entry is the origin.
	domain, _, _ = strings.Cut(domain, ",")
	domain = strings.TrimSpace(domain)
	if host, _, err := net.SplitHostPort(domain); err == nil {
		domain = host
	}
	return strings.ToLower(strings.TrimSuffix(domain, "."))
}

func WithDomainBlacklist(domains []string) BackendOpt {
	return func(b *Backend) {
		b.domainBlacklist = append([]string(nil), domains...)
	}
}

func (b *Backend) isDomainBlacklisted(ctx context.Context) bool {
	if len(b.domainBlacklist) == 0 {
		return false
	}
	// TxSource preserves the HTTP Host, including in Redis recovery records.
	domain := normalizeDomain(firstNonEmpty(GetOriginCtx(ctx), GetTxSource(ctx)))
	if domain == "" {
		return false
	}
	for _, blocked := range b.domainBlacklist {
		if normalizeDomain(blocked) == domain {
			return true
		}
	}
	return false
}

func (b *Backend) shouldSkipSubmission(ctx context.Context, reqs []*RPCReq) bool {
	if !b.isDomainBlacklisted(ctx) {
		return false
	}
	for _, req := range reqs {
		if req.Method == "eth_sendRawTransaction" || req.Method == "eth_sendBundle" {
			return true
		}
	}
	return false
}

func (b *Backend) logDomainSkip(ctx context.Context, group, method string) {
	log.Info("skipping backend for blacklisted domain", "backend_group", group, "backend", b.Name,
		"domain", firstNonEmpty(GetOriginCtx(ctx), GetTxSource(ctx)), "method", method, "req_id", GetReqID(ctx))
}

func (bg *BackendGroup) hasDomainFilteredSubmission(ctx context.Context, reqs []*RPCReq) bool {
	for _, backend := range bg.Backends {
		if backend.shouldSkipSubmission(ctx, reqs) {
			return true
		}
	}
	return false
}

func (bg *BackendGroup) submissionBackends(ctx context.Context, reqs []*RPCReq) []*Backend {
	eligible := make([]*Backend, 0, len(bg.Backends))
	for _, backend := range bg.Backends {
		if backend.shouldSkipSubmission(ctx, reqs) {
			backend.logDomainSkip(ctx, bg.Name, reqs[0].Method)
		} else {
			eligible = append(eligible, backend)
		}
	}
	return eligible
}

func skippedSubmissionResponses(reqs []*RPCReq) []*RPCRes {
	responses := make([]*RPCRes, len(reqs))
	for i, req := range reqs {
		responses[i] = NewRPCRes(req.ID, nil)
	}
	return responses
}
