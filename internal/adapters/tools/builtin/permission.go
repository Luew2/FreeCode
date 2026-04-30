package builtin

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Luew2/FreeCode/internal/core/permission"
)

type StaticPermissionGate struct {
	Policy permission.Policy
}

func NewStaticPermissionGate(policy permission.Policy) StaticPermissionGate {
	return StaticPermissionGate{Policy: policy}
}

func (g StaticPermissionGate) Decide(ctx context.Context, request permission.Request) (permission.Decision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	decision := g.Policy.DecisionFor(request.Action)
	// Path scopes apply uniformly to filesystem actions: a session that
	// restricts AllowedPaths/DeniedPaths must restrict reads as well as
	// writes, otherwise a "scoped" agent can still exfiltrate everything.
	if decision == permission.DecisionAllow && (request.Action == permission.ActionWrite || request.Action == permission.ActionRead) && !pathPolicyAllows(g.Policy, request.Subject) {
		return permission.DecisionDeny, nil
	}
	return decision, nil
}

func pathPolicyAllows(policy permission.Policy, subject string) bool {
	paths := strings.Split(subject, ",")
	for _, path := range paths {
		path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if path == "." || path == "" {
			continue
		}
		for _, denied := range policy.DeniedPaths {
			if pathMatchesScope(path, denied) {
				return false
			}
		}
		if len(policy.AllowedPaths) == 0 {
			continue
		}
		allowed := false
		for _, scope := range policy.AllowedPaths {
			if pathMatchesScope(path, scope) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func pathMatchesScope(path string, scope string) bool {
	scope = filepath.ToSlash(filepath.Clean(strings.TrimSpace(scope)))
	if scope == "." || scope == "" {
		return true
	}
	if strings.HasPrefix(scope, "**/") {
		tail := strings.TrimPrefix(scope, "**/")
		if pathMatchesScope(path, tail) {
			return true
		}
		if ok, _ := filepath.Match(tail, filepath.Base(path)); ok {
			return true
		}
		return strings.HasSuffix(path, "/"+tail)
	}
	if strings.ContainsAny(scope, "*?[") {
		if ok, _ := filepath.Match(scope, path); ok {
			return true
		}
		if ok, _ := filepath.Match(scope, filepath.Base(path)); ok {
			return true
		}
	}
	return path == scope || strings.HasPrefix(path, strings.TrimSuffix(scope, "/")+"/")
}
