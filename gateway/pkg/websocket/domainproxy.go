// pkg/websocket/domainproxy.go

package websocket

import (
	"regexp"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// DomainRewriteProxy handles domain rewriting for SIP messages
type DomainRewriteProxy struct {
	logger   *zap.Logger
	mappings map[string]string // Maps client connection IDs to their domain mappings
	mu       sync.RWMutex
}

// Domain patterns in SIP messages
var (
	fromHeaderRegex    = regexp.MustCompile(`From:[^\r\n]*sip:([^@]+)@([^;>]+)`)
	toHeaderRegex      = regexp.MustCompile(`To:[^\r\n]*sip:([^@]+)@([^;>]+)`)
	contactHeaderRegex = regexp.MustCompile(`Contact:[^\r\n]*sip:([^@]+)@([^;>]+)`)
	requestURIRegex    = regexp.MustCompile(`^[A-Z]+ sip:([^@]+)@([^;: ]+)`)
)

// NewDomainRewriteProxy creates a new proxy for domain rewriting
func NewDomainRewriteProxy(logger *zap.Logger) *DomainRewriteProxy {
	return &DomainRewriteProxy{
		logger:   logger,
		mappings: make(map[string]string),
	}
}

// SetDomainMapping sets a domain mapping for a client
func (p *DomainRewriteProxy) SetDomainMapping(clientID string, platformDomain, backendDomain string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := clientID + ":" + platformDomain
	p.mappings[key] = backendDomain

	p.logger.Debug("Set domain mapping",
		zap.String("clientID", clientID),
		zap.String("platform", platformDomain),
		zap.String("backend", backendDomain))
}

// RewriteClientToBackend rewrites a message from client to backend
func (p *DomainRewriteProxy) RewriteClientToBackend(clientID string, message []byte) []byte {
	// Add detailed logging
	p.logger.Info("Domain rewrite called",
		zap.String("clientID", clientID),
		zap.Int("messageLength", len(message)),
		zap.String("messagePreview", string(message[:min(100, len(message))])))

	// Extract domains from message
	msgStr := string(message)

	// Check if it's a SIP message
	if !isSipMessage(msgStr) {
		p.logger.Debug("Not a SIP message, skipping rewrite",
			zap.String("clientID", clientID))
		return message
	}

	// Log full SIP headers for debugging
	lines := strings.Split(msgStr, "\r\n")
	p.logger.Debug("Full SIP message headers",
		zap.String("clientID", clientID),
		zap.Strings("headers", lines[:min(15, len(lines))]))

	// Get source domain from the message
	sourceDomain := extractDomainFromSIP(msgStr)
	if sourceDomain == "" {
		p.logger.Debug("Could not extract source domain, skipping rewrite",
			zap.String("clientID", clientID))
		return message
	}

	// Get target domain for this client
	p.mu.RLock()
	targetDomain, exists := p.mappings[clientID+":"+sourceDomain]
	p.mu.RUnlock()

	if !exists {
		p.logger.Debug("No domain mapping exists for client, skipping rewrite",
			zap.String("clientID", clientID),
			zap.String("sourceDomain", sourceDomain))
		return message
	}

	// Rewrite the domain
	p.logger.Debug("Original SIP message before rewriting",
		zap.String("sourceDomain", sourceDomain),
		zap.String("targetDomain", targetDomain),
		zap.String("message", msgStr[:min(200, len(msgStr))]))

	newMsg := rewriteSIPDomains(msgStr, sourceDomain, targetDomain)

	// Log the rewrite details
	p.logger.Info("Domain rewritten successfully",
		zap.String("clientID", clientID),
		zap.String("from", sourceDomain),
		zap.String("to", targetDomain),
		zap.Int("originalLength", len(message)),
		zap.Int("newLength", len(newMsg)))

	// Log first line of rewritten message to verify Request-URI
	rewrittenLines := strings.Split(newMsg, "\r\n")
	if len(rewrittenLines) > 0 {
		p.logger.Debug("Rewritten Request-URI",
			zap.String("clientID", clientID),
			zap.String("requestLine", rewrittenLines[0]))
	}

	return []byte(newMsg)
}

// RewriteBackendToClient rewrites a message from backend to client
func (p *DomainRewriteProxy) RewriteBackendToClient(clientID string, message []byte) []byte {
	// Similar logic but in reverse direction
	// ...
	return message
}

// Helper functions
func isSipMessage(msg string) bool {
	firstLine := strings.Split(msg, "\r\n")[0]
	return strings.HasPrefix(firstLine, "REGISTER ") ||
		strings.HasPrefix(firstLine, "INVITE ") ||
		strings.HasPrefix(firstLine, "OPTIONS ") ||
		strings.HasPrefix(firstLine, "BYE ") ||
		strings.HasPrefix(firstLine, "CANCEL ") ||
		strings.HasPrefix(firstLine, "ACK ")
}

func extractDomainFromSIP(msg string) string {
	// Extract from From header
	if matches := fromHeaderRegex.FindStringSubmatch(msg); len(matches) >= 3 {
		return matches[2]
	}

	// Extract from Request-URI
	if matches := requestURIRegex.FindStringSubmatch(msg); len(matches) >= 3 {
		return matches[2]
	}

	return ""
}

func rewriteSIPDomains(msg, sourceDomain, targetDomain string) string {
	// More aggressive domain replacement - enhanced version
	newMsg := msg

	// First rewrite the top line (Request-URI) which is most critical for OpenSIPS
	lines := strings.Split(newMsg, "\r\n")
	if len(lines) > 0 {
		firstLine := lines[0]

		// Handle all SIP methods
		methodRegex := regexp.MustCompile(`^(REGISTER|INVITE|OPTIONS|BYE|CANCEL|ACK|SUBSCRIBE|NOTIFY|PUBLISH|MESSAGE)\s+sip:([^@\s]*@)?` + regexp.QuoteMeta(sourceDomain) + `(\s+SIP/2\.0)`)
		if methodRegex.MatchString(firstLine) {
			newFirstLine := methodRegex.ReplaceAllString(firstLine, "${1} sip:${2}"+targetDomain+"${3}")
			lines[0] = newFirstLine
			newMsg = strings.Join(lines, "\r\n")
		}
	}

	// Common replacements
	newMsg = strings.Replace(newMsg, "@"+sourceDomain, "@"+targetDomain, -1)
	newMsg = strings.Replace(newMsg, "sip:"+sourceDomain, "sip:"+targetDomain, -1)
	newMsg = strings.Replace(newMsg, "<sip:"+sourceDomain, "<sip:"+targetDomain, -1)
	newMsg = strings.Replace(newMsg, ";domain="+sourceDomain, ";domain="+targetDomain, -1)

	// Advanced header-specific replacements with more precise targeting

	// Replace in Contact header (all variations)
	contactRegex := regexp.MustCompile(`(?i)(Contact:|m:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = contactRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace in From header (all variations)
	fromRegex := regexp.MustCompile(`(?i)(From:|f:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = fromRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace in To header (all variations)
	toRegex := regexp.MustCompile(`(?i)(To:|t:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = toRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace in P-Asserted-Identity header
	paiRegex := regexp.MustCompile(`(?i)(P-Asserted-Identity:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = paiRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace in Route/Record-Route headers
	routeRegex := regexp.MustCompile(`(?i)(Route:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = routeRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	recordRouteRegex := regexp.MustCompile(`(?i)(Record-Route:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = recordRouteRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace in Path header
	pathRegex := regexp.MustCompile(`(?i)(Path:)([^\r\n]*sip:[^@\r\n]+@)` + regexp.QuoteMeta(sourceDomain))
	newMsg = pathRegex.ReplaceAllString(newMsg, "${1}${2}"+targetDomain)

	// Replace Domain in Host parameter
	hostRegex := regexp.MustCompile(`;host=` + regexp.QuoteMeta(sourceDomain))
	newMsg = hostRegex.ReplaceAllString(newMsg, ";host="+targetDomain)

	// Replace domain in SIP URI parameters
	paramRegex := regexp.MustCompile(`;domain=` + regexp.QuoteMeta(sourceDomain))
	newMsg = paramRegex.ReplaceAllString(newMsg, ";domain="+targetDomain)

	// Replace AOR (Address of Record) in various headers
	aorRegex := regexp.MustCompile(`<(sip:[^@>]+@)` + regexp.QuoteMeta(sourceDomain) + `>`)
	newMsg = aorRegex.ReplaceAllString(newMsg, "<${1}"+targetDomain+">")

	// Replace domain in any remaining parts (careful with this)
	domainRegex := regexp.MustCompile(`([^A-Za-z0-9._-])` + regexp.QuoteMeta(sourceDomain) + `([^A-Za-z0-9._-])`)
	newMsg = domainRegex.ReplaceAllString(newMsg, "${1}"+targetDomain+"${2}")

	return newMsg
}

// Helper function for logging
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
