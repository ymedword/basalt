package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// Called only after the business authentication middleware has succeeded.
// The subject identifies the presented credential by a one-way fingerprint.
func knowledgeRequestActor(r *http.Request) core.Actor {
	if token, ok := bearerToken(r); ok {
		digest := sha256.Sum256([]byte(token))
		return core.Actor{Subject: core.ActorSubjectTokenPrefix + hex.EncodeToString(digest[:])[:16], Method: core.AuthMethodBearer, Source: r.RemoteAddr}
	}
	return core.Actor{Subject: core.ActorSubjectLocalTrust, Method: core.AuthMethodLocalTrust, Source: r.RemoteAddr}
}
