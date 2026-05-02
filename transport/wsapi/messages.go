// Package wsapi provides an optional WebSocket transport adapter for agentwatch.
//
// The WebSocket protocol wraps monitor.Event — it does not define separate
// message types. Clients receive events as JSON-encoded monitor.Event values.
package wsapi
