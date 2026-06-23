package responsesconv

import "strings"

// This file ports the content-part and message construction helpers used by
// the request converter (convertResponsesRequestToChatCompletions). These turn
// Responses-API input items into Chat Completions messages.

// chatTextPart builds {"type":"text","text":text}.
func chatTextPart(text string) Obj { return Obj{"type": "text", "text": text} }

func convertImageURL(v interface{}) interface{} {
	if s, ok := asString(v); ok {
		return Obj{"url": s}
	}
	if isRecord(v) {
		return v
	}
	return nil
}

// convertResponsesContentPartToChat maps a single Responses content part to a
// Chat Completions part. Returns nil to drop the part. Throws (returns error)
// for unsupported parts (input_file) or malformed image parts.
func convertResponsesContentPartToChat(part interface{}, param string) (interface{}, error) {
	if s, ok := asString(part); ok {
		// Bare strings are handled by the caller; here a string part is text.
		return s, nil
	}
	p := toObj(part)
	if p == nil {
		return nil, nil
	}

	switch p["type"] {
	case "input_text", "output_text", "text":
		if text, ok := strField(p, "text"); ok {
			return chatTextPart(text), nil
		}
		return nil, nil
	case "refusal":
		if refusal, ok := strField(p, "refusal"); ok {
			return chatTextPart(refusal), nil
		}
		return nil, nil
	case "input_image", "image_url":
		img := convertImageURL(p["image_url"])
		if img == nil {
			return nil, newError("Responses image content requires image_url to be a string or object.", param, "")
		}
		return Obj{"type": "image_url", "image_url": img}, nil
	case "input_file":
		return nil, newError("Responses input_file content cannot be represented by Chat Completions.", param, "")
	}

	// Fallback: use .text if present.
	if text, ok := strField(p, "text"); ok {
		return chatTextPart(text), nil
	}
	return nil, nil
}

// convertResponsesContentToChat converts a Responses message "content" field
// into a Chat content value (string or array of parts).
func convertResponsesContentToChat(content interface{}, role string, param string) (interface{}, error) {
	if s, ok := asString(content); ok {
		return s, nil
	}
	if content == nil {
		if role == "assistant" {
			return nil, nil
		}
		return "", nil
	}

	arr := toArr(content)
	if arr == nil {
		// Non-array object with a .text string → return that string.
		if isRecord(content) {
			if text, ok := strField(content.(Obj), "text"); ok {
				return text, nil
			}
		}
		// Fallback to JSON-encoded string (TS: String(content)).
		return normalizeFunctionArguments(content), nil
	}

	var converted []interface{}
	for i, item := range arr {
		c, err := convertResponsesContentPartToChat(item, sprintf("%s[%d]", param, i))
		if err != nil {
			return nil, err
		}
		if c != nil {
			converted = append(converted, c)
		}
	}

	// If every part is plain text, join into a single string.
	allText := true
	var texts []string
	for _, part := range converted {
		if s, ok := part.(string); ok {
			texts = append(texts, s)
			continue
		}
		o := toObj(part)
		if t, isText := o["type"]; isText && t == "text" {
			if text, ok := strField(o, "text"); ok {
				texts = append(texts, text)
				continue
			}
		}
		allText = false
		break
	}
	if allText {
		return strings.Join(texts, ""), nil
	}

	// Otherwise wrap bare strings into text parts.
	out := make([]interface{}, 0, len(converted))
	for _, part := range converted {
		if s, ok := part.(string); ok {
			out = append(out, chatTextPart(s))
		} else {
			out = append(out, part)
		}
	}
	return out, nil
}

func normalizeChatRole(role interface{}) string {
	s, _ := asString(role)
	switch s {
	case "developer":
		return "system"
	case "system", "user", "assistant", "tool":
		return s
	default:
		return "user"
	}
}

func convertFunctionCallItemToChatMessage(item Obj, index int) (Obj, error) {
	callID, ok := strField(item, "call_id")
	if !ok {
		callID, ok = strField(item, "id")
	}
	if !ok {
		callID = sprintf("call_%d", index)
	}
	name, ok := strField(item, "name")
	if !ok {
		return nil, newError("Responses function_call item requires a name.", sprintf("input[%d].name", index), "")
	}
	return Obj{
		"role":    "assistant",
		"content": nil,
		"tool_calls": Arr{
			Obj{
				"id":   callID,
				"type": "function",
				"function": Obj{
					"name":      name,
					"arguments": normalizeFunctionArguments(item["arguments"]),
				},
			},
		},
	}, nil
}

func convertFunctionCallOutputItemToChatMessage(item Obj, index int) (Obj, error) {
	callID, ok := strField(item, "call_id")
	if !ok {
		return nil, newError("Responses function_call_output item requires call_id.", sprintf("input[%d].call_id", index), "")
	}
	var content interface{}
	if s, ok := strField(item, "output"); ok {
		content = s
	} else {
		content = normalizeFunctionArguments(item["output"])
	}
	return Obj{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      content,
	}, nil
}

// convertResponsesInputItemToChatMessage returns nil to drop the item.
func convertResponsesInputItemToChatMessage(item interface{}, index int) (Obj, error) {
	if s, ok := asString(item); ok {
		return Obj{"role": "user", "content": s}, nil
	}
	rec := toObj(item)
	if rec == nil {
		return Obj{"role": "user", "content": normalizeFunctionArguments(item)}, nil
	}

	switch rec["type"] {
	case "reasoning":
		return nil, nil
	case "function_call":
		return convertFunctionCallItemToChatMessage(rec, index)
	case "function_call_output":
		return convertFunctionCallOutputItemToChatMessage(rec, index)
	case "item_reference":
		return nil, newError(
			"Responses item_reference requires server-side state and is not supported by Chat Completions compatibility.",
			sprintf("input[%d]", index), "")
	}

	role := normalizeChatRole(rec["role"])
	content, err := convertResponsesContentToChat(rec["content"], role, sprintf("input[%d].content", index))
	if err != nil {
		return nil, err
	}
	msg := Obj{"role": role, "content": content}

	if role == "tool" {
		callID, ok := strField(rec, "tool_call_id")
		if !ok {
			callID, _ = strField(rec, "call_id")
		}
		if callID != "" {
			msg["tool_call_id"] = callID
		}
	}

	if role == "assistant" {
		if tc := toArr(rec["tool_calls"]); tc != nil {
			msg["tool_calls"] = tc
		}
	}

	return msg, nil
}

func convertResponsesInputToChatMessages(input interface{}) ([]Obj, error) {
	if s, ok := asString(input); ok {
		return []Obj{{"role": "user", "content": s}}, nil
	}
	arr := toArr(input)
	if arr == nil {
		return nil, newError("Responses request requires input to be a string or an array for Chat Completions compatibility.", "input", "")
	}

	var messages []Obj
	for i, item := range arr {
		msg, err := convertResponsesInputItemToChatMessage(item, i)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// contentToSystemText flattens a system message's content into a string.
func contentToSystemText(content interface{}) string {
	if s, ok := asString(content); ok {
		return s
	}
	if content == nil {
		return ""
	}
	if arr := toArr(content); arr != nil {
		var parts []string
		for _, part := range arr {
			if s, ok := asString(part); ok {
				parts = append(parts, s)
				continue
			}
			if isRecord(part) {
				if text, ok := strField(part.(Obj), "text"); ok {
					parts = append(parts, text)
					continue
				}
			}
			parts = append(parts, normalizeFunctionArguments(part))
		}
		// TS filters empty strings before join.
		var nonEmpty []string
		for _, p := range parts {
			if p != "" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		return strings.Join(nonEmpty, "\n\n")
	}
	return normalizeFunctionArguments(content)
}

// mergeLeadingSystemMessages collapses a leading run of >=2 system messages
// into a single message joined by "\n\n".
func mergeLeadingSystemMessages(messages []Obj) []Obj {
	if len(messages) < 2 {
		return messages
	}
	r0, _ := strField(messages[0], "role")
	r1, _ := strField(messages[1], "role")
	if r0 != "system" || r1 != "system" {
		return messages
	}

	var systemParts []string
	idx := 0
	for idx < len(messages) {
		role, _ := strField(messages[idx], "role")
		if role != "system" {
			break
		}
		text := contentToSystemText(messages[idx]["content"])
		if text != "" {
			systemParts = append(systemParts, text)
		}
		idx++
	}

	merged := append([]Obj(nil), Obj{"role": "system", "content": strings.Join(systemParts, "\n\n")})
	merged = append(merged, messages[idx:]...)
	return merged
}
