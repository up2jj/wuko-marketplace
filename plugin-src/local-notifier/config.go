package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	deliveryAuto     = "auto"
	deliveryTerminal = "terminal"
	deliverySystem   = "system"
)

type config struct {
	Message  string  `json:"message"`
	Title    *string `json:"title"`
	Delivery string  `json:"delivery"`
}

func decodeConfig(raw json.RawMessage, allowTemplates bool) (config, error) {
	var result config
	if err := decodeRawStrict(raw, &result); err != nil {
		return config{}, fmt.Errorf("invalid local-notifier.notify configuration: %w", err)
	}
	if strings.TrimSpace(result.Message) == "" && !(allowTemplates && containsTemplate(result.Message)) {
		return config{}, fmt.Errorf("invalid local-notifier.notify configuration: message is required")
	}
	if result.Title != nil && strings.TrimSpace(*result.Title) == "" && !(allowTemplates && containsTemplate(*result.Title)) {
		return config{}, fmt.Errorf("invalid local-notifier.notify configuration: title must not be empty")
	}
	if result.Delivery == "" {
		result.Delivery = deliveryAuto
	}
	if !validDelivery(result.Delivery) && !(allowTemplates && containsTemplate(result.Delivery)) {
		return config{}, fmt.Errorf("invalid local-notifier.notify configuration: delivery must be auto, terminal, or system")
	}
	return result, nil
}

func containsTemplate(value string) bool {
	return strings.Contains(value, "{{")
}

func validDelivery(value string) bool {
	return value == deliveryAuto || value == deliveryTerminal || value == deliverySystem
}

func (c config) resolvedTitle(workflowName string) string {
	if c.Title != nil {
		return *c.Title
	}
	if strings.TrimSpace(workflowName) != "" {
		return workflowName
	}
	return "Wuko"
}
