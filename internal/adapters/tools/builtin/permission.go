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
	if decision == permission.DecisionAllow && request.Action == permission.ActionWrite && !pathPolicyAllows(g.Policy, request.Subject) {
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
	return path == scope || strings.HasPrefix(path, strings.TrimSuffix(scope, "/")+"/")
}
