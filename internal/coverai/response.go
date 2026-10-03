package coverai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type modelResponse struct {
	Text       string
	ErrorHint  string
	Incomplete bool
	Refused    bool
	Failed     bool
}

func parseModelResponse(body []byte, style APIStyle) modelResponse {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		return parseModelEventStream(trimmed, style)
	}
	var value map[string]any
	if json.Unmarshal(trimmed, &value) != nil {
		return modelResponse{}
	}
	if style == APIResponses {
		return parseResponsesJSON(value)
	}
	return parseChatJSON(value)
}

func parseResponsesJSON(value map[string]any) modelResponse {
	var response modelResponse
	topLevelText := stringValue(value["output_text"])
	response.Text = topLevelText
	for _, item := range anySlice(value["output"]) {
		message := mapValue(item)
		for _, part := range anySlice(message["content"]) {
			content := mapValue(part)
			switch stringValue(content["type"]) {
			case "refusal":
				response.Refused = true
			case "output_text", "text":
				if topLevelText == "" {
					response.Text += stringValue(content["text"])
				}
			}
		}
	}
	if refusal := stringValue(value["refusal"]); refusal != "" {
		response.Refused = true
	}
	status := strings.ToLower(stringValue(value["status"]))
	response.Incomplete = status == "incomplete" ||
		mapValue(value["incomplete_details"]) != nil
	response.Failed = status == "failed" || value["error"] != nil
	if response.Failed {
		response.ErrorHint = providerErrorHint(value)
	}
	return response
}

func parseChatJSON(value map[string]any) modelResponse {
	var response modelResponse
	for _, choice := range anySlice(value["choices"]) {
		choiceMap := mapValue(choice)
		message := mapValue(choiceMap["message"])
		response.Text += textContent(message["content"])
		if stringValue(message["refusal"]) != "" {
			response.Refused = true
		}
		finishReason := strings.ToLower(stringValue(choiceMap["finish_reason"]))
		if finishReason == "length" || finishReason == "max_tokens" {
			response.Incomplete = true
		}
		if finishReason == "error" {
			response.Failed = true
		}
	}
	if err := mapValue(value["error"]); err != nil {
		response.Failed = true
		response.ErrorHint = providerErrorHint(value)
	}
	return response
}

func parseModelEventStream(body []byte, style APIStyle) modelResponse {
	var response modelResponse
	var builder strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var eventName string
	var dataLines []string
	processEvent := func() {
		if len(dataLines) == 0 {
			eventName = ""
			return
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if data == "[DONE]" {
			eventName = ""
			return
		}
		var value map[string]any
		if json.Unmarshal([]byte(data), &value) != nil {
			eventName = ""
			return
		}
		kind := stringValue(value["type"])
		if kind == "" {
			kind = eventName
		}
		switch {
		case kind == "response.output_text.delta":
			delta := stringValue(value["delta"])
			if delta != "" {
				builder.WriteString(delta)
			}
		case kind == "response.output_text.done":
			if builder.Len() == 0 {
				builder.WriteString(stringValue(value["text"]))
			}
		case kind == "response.refusal.delta":
			response.Refused = true
		case kind == "response.completed":
			final := parseResponsesJSON(mapValue(value["response"]))
			if builder.Len() == 0 {
				builder.WriteString(final.Text)
			}
			response.Refused = response.Refused || final.Refused
			response.Failed = response.Failed || final.Failed
			response.Incomplete = response.Incomplete || final.Incomplete
		case kind == "response.incomplete":
			response.Incomplete = true
		case kind == "response.failed" || kind == "error":
			response.Failed = true
			response.ErrorHint = providerErrorHint(value)
			if response.ErrorHint == "" {
				response.ErrorHint = providerErrorHint(mapValue(value["response"]))
			}
		case strings.HasPrefix(kind, "response.") && strings.Contains(kind, "refusal"):
			response.Refused = true
		case style == APIChat && (kind == "chat.completion.chunk" || strings.Contains(kind, "chunk")):
			for _, choice := range anySlice(value["choices"]) {
				delta := mapValue(mapValue(choice)["delta"])
				content := textContent(delta["content"])
				if content != "" {
					builder.WriteString(content)
				}
				if stringValue(delta["refusal"]) != "" {
					response.Refused = true
				}
			}
		}
		eventName = ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			processEvent()
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	processEvent()
	if scanner.Err() != nil {
		response.Failed = true
	}
	response.Text = builder.String()
	return response
}

func providerErrorHint(value map[string]any) string {
	err := mapValue(value["error"])
	if err == nil {
		return ""
	}
	parts := []string{
		stringValue(err["code"]),
		stringValue(err["type"]),
		stringValue(err["message"]),
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func textContent(value any) string {
	if text := stringValue(value); text != "" {
		return text
	}
	var result strings.Builder
	for _, part := range anySlice(value) {
		content := mapValue(part)
		if text := stringValue(content["text"]); text != "" {
			result.WriteString(text)
		}
	}
	return result.String()
}

func anySlice(value any) []any {
	out, _ := value.([]any)
	return out
}
