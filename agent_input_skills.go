package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

var inputSkillIDPattern = regexp.MustCompile(`^skl_[a-zA-Z0-9_-]{1,124}$`)
var inputSkillRevisionPattern = regexp.MustCompile(`^skr_[a-zA-Z0-9_-]{1,124}$`)
var inputSkillHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func invalidInputSkills() error {
	return &RPCError{Code: CodeInvalidParams, Message: "invalid input_skills"}
}

// Validate shape here; verifying and installing actual bytes is the opted-in
// handler's duty. The raw boundary preserves null/omitted distinctions.
func (r *agentRuntime) turnStartRaw(ctx context.Context, raw json.RawMessage) (any, error) {
	var envelope struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, invalidInputSkills()
	}
	if _, present := envelope.Config["input_skill_receipt"]; present {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "input_skill_receipt is effective-only"}
	}
	if skills, present := envelope.Config["input_skills"]; present {
		var documents []InputSkillDocument
		decoder := json.NewDecoder(bytes.NewReader(skills))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&documents) != nil || len(documents) == 0 || validateInputSkillDocuments(documents) != nil {
			return nil, invalidInputSkills()
		}
	}
	return decodeThen(r.turnStart)(ctx, raw)
}
func validateInputSkillDocuments(documents []InputSkillDocument) error {
	if len(documents) > 10 {
		return invalidInputSkills()
	}
	seen := map[string]bool{}
	total := 0
	for _, doc := range documents {
		total += len(doc.Body)
		if !inputSkillIDPattern.MatchString(doc.SkillID) || !inputSkillRevisionPattern.MatchString(doc.RevisionID) ||
			!inputSkillHashPattern.MatchString(doc.BodySHA256) || strings.TrimSpace(doc.Name) == "" || len(doc.Name) > 255 ||
			strings.TrimSpace(doc.Body) == "" || len(doc.Body) > 65536 || total > 262144 || seen[doc.SkillID] {
			return invalidInputSkills()
		}
		seen[doc.SkillID] = true
	}
	return nil
}
