package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

var (
	allowedToolCallKeys = map[string]struct{}{
		"tool":       {},
		"name":       {},
		"args":       {},
		"arguments":  {},
		"input":      {},
		"parameters": {},
		"tool_input": {},
		"function":   {},
		"type":       {},
		"id":         {},
		"call_id":    {},
	}
	allowedFunctionKeys = map[string]struct{}{
		"name":      {},
		"arguments": {},
	}
)

func decodeJSONStrict(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON content")
	}
	return nil
}

func normalizeToolArgs(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	// Support OpenAI-style stringified arguments for compatibility.
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
		}
		trimmed = bytes.TrimSpace([]byte(encoded))
		if len(trimmed) == 0 {
			return json.RawMessage(`{}`), nil
		}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	return json.RawMessage(trimmed), nil
}

func firstUnknownKey(m map[string]json.RawMessage, allowed map[string]struct{}) string {
	for k := range m {
		if _, ok := allowed[k]; !ok {
			return k
		}
	}
	return ""
}

func extractStringField(raw json.RawMessage, field string) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return strings.TrimSpace(value), nil
}

// parseAllToolCalls scans all lines of s and collects every TOOL_CALL entry.
func parseAllToolCalls(s string) (calls []toolCall, parseErrs []error) {
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, toolCallPrefix) {
			continue
		}

		var candidate strings.Builder
		candidate.WriteString(line)
		var (
			parsed  bool
			lastErr error
			lastJ   = i
		)
		for j := i; j < len(lines); j++ {
			if j > i {
				nextLine := strings.TrimSpace(lines[j])
				if strings.HasPrefix(nextLine, toolCallPrefix) || strings.HasPrefix(nextLine, finalPrefix) {
					break
				}
				candidate.WriteString("\n" + lines[j])
			}
			call, hasCall, err := parseToolCall(strings.TrimSpace(candidate.String()))
			if err == nil && hasCall {
				calls = append(calls, call)
				i = j
				parsed = true
				break
			}
			if hasCall {
				lastErr = err
			}
			lastJ = j
		}
		if parsed {
			continue
		}
		if lastErr != nil {
			parseErrs = append(parseErrs, lastErr)
		}
		i = lastJ
	}
	return calls, parseErrs
}

func parseToolCall(s string) (toolCall, bool, error) {
	if !strings.HasPrefix(s, toolCallPrefix) {
		return toolCall{}, false, nil
	}
	raw := strings.TrimSpace(strings.TrimPrefix(s, toolCallPrefix))
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return toolCall{}, true, fmt.Errorf("invalid tool call JSON: %w", err)
	}
	if unknown := firstUnknownKey(envelope, allowedToolCallKeys); unknown != "" {
		return toolCall{}, true, fmt.Errorf("unsupported tool call field %q", unknown)
	}

	toolFromTool, err := extractStringField(envelope["tool"], "tool")
	if err != nil {
		return toolCall{}, true, err
	}
	toolFromName, err := extractStringField(envelope["name"], "name")
	if err != nil {
		return toolCall{}, true, err
	}
	toolName := toolFromTool
	if toolName == "" {
		toolName = toolFromName
	} else if toolFromName != "" && toolFromName != toolFromTool {
		return toolCall{}, true, fmt.Errorf("conflicting tool names %q and %q", toolFromTool, toolFromName)
	}

	var fn map[string]json.RawMessage
	if fnRaw, ok := envelope["function"]; ok && len(bytes.TrimSpace(fnRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(fnRaw), []byte("null")) {
		if err := json.Unmarshal(fnRaw, &fn); err != nil {
			return toolCall{}, true, fmt.Errorf("function must be an object")
		}
		if unknown := firstUnknownKey(fn, allowedFunctionKeys); unknown != "" {
			return toolCall{}, true, fmt.Errorf("unsupported function field %q", unknown)
		}
		fnName, err := extractStringField(fn["name"], "function.name")
		if err != nil {
			return toolCall{}, true, err
		}
		if toolName == "" {
			toolName = fnName
		} else if fnName != "" && fnName != toolName {
			return toolCall{}, true, fmt.Errorf("conflicting function name %q and tool name %q", fnName, toolName)
		}
	}

	if toolName == "" {
		return toolCall{}, true, fmt.Errorf("tool call missing tool name")
	}

	var argKeys []string
	for _, key := range []string{"args", "arguments", "input", "parameters", "tool_input"} {
		if rawArgs, ok := envelope[key]; ok && len(bytes.TrimSpace(rawArgs)) > 0 && !bytes.Equal(bytes.TrimSpace(rawArgs), []byte("null")) {
			argKeys = append(argKeys, key)
		}
	}
	if rawFnArgs, ok := fn["arguments"]; ok && len(bytes.TrimSpace(rawFnArgs)) > 0 && !bytes.Equal(bytes.TrimSpace(rawFnArgs), []byte("null")) {
		argKeys = append(argKeys, "function.arguments")
	}
	if len(argKeys) > 1 {
		return toolCall{}, true, fmt.Errorf("ambiguous arguments fields: %s", strings.Join(argKeys, ", "))
	}

	rawArgs := json.RawMessage(`{}`)
	switch {
	case len(argKeys) == 0:
		// leave defaults
	case argKeys[0] == "function.arguments":
		rawArgs = fn["arguments"]
	default:
		rawArgs = envelope[argKeys[0]]
	}
	normalizedArgs, err := normalizeToolArgs(rawArgs)
	if err != nil {
		return toolCall{}, true, err
	}

	return toolCall{Tool: toolName, Args: normalizedArgs}, true, nil
}

func parseFinal(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, finalPrefix) {
		return "", false
	}
	out := strings.TrimSpace(strings.TrimPrefix(trimmed, finalPrefix))
	out = strings.TrimPrefix(out, ":")
	out = strings.TrimSpace(out)
	return out, true
}
