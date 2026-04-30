package mcp

import "encoding/json"

const (
	protocolVersion = "2025-11-25"

	methodInitialize       = "initialize"
	methodInitialized      = "notifications/initialized"
	methodToolsList        = "tools/list"
	methodToolsCall        = "tools/call"
	methodNotificationsLog = "notifications/message"
)

type requestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      implementation     `json:"clientInfo"`
}

type clientCapabilities struct{}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ServerInfo      implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
	Meta            map[string]any `json:"_meta,omitempty"`
}

type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type listToolsResult struct {
	Tools      []tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type callToolResult struct {
	Content           []contentBlock `json:"content,omitempty"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

type contentBlock struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Data     string         `json:"data,omitempty"`
	MIMEType string         `json:"mimeType,omitempty"`
	URI      string         `json:"uri,omitempty"`
	Name     string         `json:"name,omitempty"`
	Resource map[string]any `json:"resource,omitempty"`
}
