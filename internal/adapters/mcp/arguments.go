package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errBadArgument marks an argument this server will not act on.
//
// A sentinel so the transport can tell "you asked for something impossible"
// from "the server failed": the first is an error the agent can fix by calling
// again with different arguments, and the second is not. Both come back as a
// tool error rather than a protocol error, because a protocol error aborts the
// agent's turn and a bad argument should cost it one retry.
var errBadArgument = errors.New("invalid argument")

// arguments is one tool call's parameters.
//
// A decoded map rather than a struct per tool, because the table in tools.go
// is data and a table whose entries each carried their own Go type could not
// be a table at all — it would be ten registrations with ten signatures, and
// the property that `tools/list` is one readable list would be gone.
//
// The cost is that validation is by hand. It is paid back in the errors:
// every message below names the argument, what arrived, and what the tool
// wanted, because the reader is a model that will otherwise try the same call
// again with the same value.
type arguments struct {
	values map[string]any
}

// decodeArguments reads a tools/call payload.
//
// An absent or null `arguments` is an empty set rather than an error: a tool
// with no required parameters is legitimately called with nothing, and
// different clients spell "nothing" as `{}`, as `null` and as an omitted key.
func decodeArguments(raw json.RawMessage) (arguments, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return arguments{values: map[string]any{}}, nil
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return arguments{}, fmt.Errorf("%w: the arguments are not a JSON object: %w",
			errBadArgument, err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return arguments{values: values}, nil
}

// String reads a text argument, accepting a number where a string was asked
// for.
//
// The leniency is deliberate and has one cause: `project` takes an id or a
// slug, and a model that has just read `"id": 3` out of list_projects will
// send `3` rather than `"3"` about half the time. Refusing it would be
// technically correct and would cost a round trip every time, so a scalar is
// rendered as the text it obviously is. Anything that is not a scalar — an
// object, an array — is still refused, because there is no obvious text for it.
func (a arguments) String(name string) string {
	switch value := a.values[name].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

// Int reads a whole-number argument, reporting whether it was there at all.
//
// The three-value return distinguishes "absent" from "zero", which matters for
// every optional `limit`: a tool that could not tell them apart would send
// `limit=0` on every call that omitted it, and the API rejects a limit of zero
// because zero results is not a page anybody asked for.
func (a arguments) Int(name string) (value int64, found bool, err error) {
	switch typed := a.values[name].(type) {
	case nil:
		return 0, false, nil
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false, fmt.Errorf("%w: %s is %v, which is not a whole number",
				errBadArgument, name, typed)
		}
		return int64(typed), true, nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false, nil
		}
		parsed, parseErr := strconv.ParseInt(trimmed, 10, 64)
		if parseErr != nil {
			return 0, false, fmt.Errorf("%w: %s is %q, which is not a number",
				errBadArgument, name, typed)
		}
		return parsed, true, nil
	default:
		return 0, false, fmt.Errorf("%w: %s is %T, and a number was expected",
			errBadArgument, name, typed)
	}
}

// Bool reads a flag. Anything absent is false, which is what every flag in
// this table means when it is left out.
func (a arguments) Bool(name string) bool {
	switch typed := a.values[name].(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && parsed
	default:
		return false
	}
}

// The schema builders.
//
// They exist so a tool's schema reads as a description of its arguments
// rather than as nested map literals, and so the three shapes this table uses
// are spelled the same way every time. JSON Schema 2020-12, which is the draft
// MCP clients expect.

// object builds the argument schema of one tool.
func object(properties map[string]any, required []string) json.RawMessage {
	schema := map[string]any{"type": "object"}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	// Unknown arguments are refused by the client rather than silently
	// dropped here, for the reason the REST API rejects unknown JSON fields:
	// a misspelled parameter that is ignored produces a call that succeeds
	// and answers the wrong question.
	schema["additionalProperties"] = false

	encoded, err := json.Marshal(schema)
	if err != nil {
		// The input is a literal in this package; a failure here is a
		// programming error that must not reach a running server quietly.
		panic("encoding a tool schema: " + err.Error())
	}
	return encoded
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerProperty(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func booleanProperty(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func enumProperty(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}
