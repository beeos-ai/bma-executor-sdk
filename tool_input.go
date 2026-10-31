package bmaexecutor

import "encoding/json"

// DecodeInput decodes tool.call input into the built-in ToolInput shape. The
// wire keeps input raw because each tool defines which members it reads.
func (p ToolCallParams) DecodeInput() (ToolInput, error) {
	var input ToolInput
	err := json.Unmarshal(p.Input, &input)
	return input, err
}
