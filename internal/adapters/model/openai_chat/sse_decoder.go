package openai_chat

import (
	"bufio"
	"bytes"
	"io"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

// tolerantSSEDecoder is a drop-in replacement for the openai-go SDK's
// stock ssestream.Decoder. The stock decoder dispatches an event on every
// blank line, even when no `data:` line has been observed since the last
// dispatch — meaning a "comment + blank line" provider keepalive becomes
// an empty-payload json.Unmarshal in the SDK's Stream wrapper, which fails
// with "unexpected end of JSON input".
//
// Per the SSE spec (whatwg server-sent-events), an event without any data
// MUST be ignored. This decoder follows the spec.
//
// We also strip the `event:` field for empty/unknown event types so the
// SDK's Stream wrapper doesn't try to synthesize a "{event:..., data:...}"
// envelope for non-thread events.
type tolerantSSEDecoder struct {
	rc      io.ReadCloser
	scanner *bufio.Scanner
	evt     ssestream.Event
	err     error
}

func newTolerantSSEDecoder(rc io.ReadCloser) ssestream.Decoder {
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(nil, bufio.MaxScanTokenSize<<9)
	return &tolerantSSEDecoder{rc: rc, scanner: scanner}
}

func (d *tolerantSSEDecoder) Next() bool {
	if d.err != nil {
		return false
	}

	var (
		event   string
		data    bytes.Buffer
		hasData bool
	)

	for d.scanner.Scan() {
		raw := d.scanner.Bytes()

		if len(raw) == 0 {
			if !hasData {
				// SSE spec: ignore empty events. Reset and keep scanning.
				event = ""
				continue
			}
			d.evt = ssestream.Event{Type: event, Data: data.Bytes()}
			return true
		}

		name, value, _ := bytes.Cut(raw, []byte(":"))
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}

		switch string(name) {
		case "":
			// Comment line, ignore.
			continue
		case "event":
			event = string(value)
		case "data":
			if _, err := data.Write(value); err != nil {
				d.err = err
				return false
			}
			if _, err := data.WriteRune('\n'); err != nil {
				d.err = err
				return false
			}
			hasData = true
		}
	}

	if err := d.scanner.Err(); err != nil {
		d.err = err
		return false
	}

	if hasData {
		d.evt = ssestream.Event{Type: event, Data: data.Bytes()}
		return true
	}

	return false
}

func (d *tolerantSSEDecoder) Event() ssestream.Event {
	return d.evt
}

func (d *tolerantSSEDecoder) Close() error {
	return d.rc.Close()
}

func (d *tolerantSSEDecoder) Err() error {
	return d.err
}
