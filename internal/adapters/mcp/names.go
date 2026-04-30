package mcp

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const maxToolNameLength = 64

type ToolName struct {
	PublicName string
	ServerName string
	ToolName   string
}

func publicToolName(serverName string, prefix string, toolName string) (string, error) {
	cleanPrefix := sanitizeName(firstNonEmpty(prefix, serverName))
	cleanTool := sanitizeName(toolName)
	if cleanPrefix == "" {
		return "", fmt.Errorf("MCP server %q tools_prefix must contain at least one letter or digit", serverName)
	}
	if cleanTool == "" {
		return "", fmt.Errorf("MCP server %q exposed tool %q with no valid public name", serverName, toolName)
	}
	name := "mcp_" + cleanPrefix + "_" + cleanTool
	if len(name) <= maxToolNameLength {
		return name, nil
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	keep := maxToolNameLength - len(digest) - 1
	if keep < len("mcp_x_") {
		keep = len("mcp_x_")
	}
	return strings.TrimRight(name[:keep], "_-") + "_" + digest, nil
}

func sanitizeName(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastUnderscore = false
		case r == '_' || r == '-' || r == '.' || r == ' ':
			if !lastUnderscore && builder.Len() > 0 {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
