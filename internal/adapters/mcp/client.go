package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	name        string
	version     string
	transport   *stdioTransport
	callTimeout time.Duration

	nextID  atomic.Int64
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan responseEnvelope
	closed  bool
	lastErr string
}

func newClient(name string, version string, transport *stdioTransport, callTimeout time.Duration) *Client {
	client := &Client{
		name:        name,
		version:     version,
		transport:   transport,
		callTimeout: callTimeout,
		pending:     map[int64]chan responseEnvelope{},
	}
	go client.readLoop()
	return client
}

func (c *Client) Initialize(ctx context.Context) (initializeResult, error) {
	var result initializeResult
	err := c.request(ctx, methodInitialize, initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    clientCapabilities{},
		ClientInfo: implementation{
			Name:    "freecode",
			Version: c.version,
		},
	}, &result)
	if err != nil {
		return initializeResult{}, err
	}
	if err := c.notify(ctx, methodInitialized, map[string]any{}); err != nil {
		return initializeResult{}, err
	}
	return result, nil
}

func (c *Client) ListTools(ctx context.Context) ([]tool, error) {
	var all []tool
	cursor := ""
	for {
		var result listToolsResult
		params := listToolsParams{Cursor: cursor}
		if err := c.request(ctx, methodToolsList, params, &result); err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		cursor = strings.TrimSpace(result.NextCursor)
		if cursor == "" {
			return all, nil
		}
	}
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (callToolResult, error) {
	var result callToolResult
	err := c.request(ctx, methodToolsCall, callToolParams{Name: name, Arguments: args}, &result)
	if err != nil {
		return callToolResult{}, err
	}
	return result, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.mu.Unlock()
	if c.transport != nil {
		return c.transport.Close()
	}
	return nil
}

func (c *Client) Stderr() string {
	if c == nil || c.transport == nil {
		return ""
	}
	return c.transport.Stderr()
}

func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func (c *Client) setLastError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

func (c *Client) request(ctx context.Context, method string, params any, target any) error {
	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	id := c.nextID.Add(1)
	ch := make(chan responseEnvelope, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("MCP client %s is closed", c.name)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(requestEnvelope{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.removePending(id)
		c.setLastError(err)
		return err
	}
	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case response, ok := <-ch:
		if !ok {
			return fmt.Errorf("MCP client %s closed before %s completed", c.name, method)
		}
		if response.Error != nil {
			err := fmt.Errorf("MCP %s error %d: %s", method, response.Error.Code, response.Error.Message)
			c.setLastError(err)
			return err
		}
		if target == nil {
			return nil
		}
		if len(response.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.Result, target); err != nil {
			err = fmt.Errorf("decode MCP %s result: %w", method, err)
			c.setLastError(err)
			return err
		}
		return nil
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return c.write(requestEnvelope{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(request requestEnvelope) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.transport == nil || c.transport.stdin == nil {
		return fmt.Errorf("MCP client %s has no stdin", c.name)
	}
	if _, err := c.transport.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write MCP request %s: %w", request.Method, err)
	}
	return nil
}

func (c *Client) readLoop() {
	if c.transport == nil || c.transport.stdout == nil {
		return
	}
	decoder := json.NewDecoder(c.transport.stdout)
	for {
		var response responseEnvelope
		if err := decoder.Decode(&response); err != nil {
			if err != io.EOF {
				c.setLastError(fmt.Errorf("read MCP response: %w", err))
			}
			c.closePending()
			return
		}
		if response.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch := c.pending[response.ID]
		delete(c.pending, response.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- response
			close(ch)
		}
	}
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) closePending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}

func timeoutDuration(ms int, fallback time.Duration) time.Duration {
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}
