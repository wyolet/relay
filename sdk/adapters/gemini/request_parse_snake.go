package gemini

// The API accepts a request's proto field names (system_instruction, inline_data, …) as well as their JSON names. Twin fields decode the proto names and these methods fold them into the JSON-name fields, which win when both are set; ParseRequest reads only the latter.

func (p *geminiPart) fillFromSnakeCase() {
	if p.ThoughtSignature == "" {
		p.ThoughtSignature = p.ThoughtSignatureSnake
	}
	if p.InlineData == nil {
		p.InlineData = p.InlineDataSnake
	}
	if p.FileData == nil {
		p.FileData = p.FileDataSnake
	}
	if p.FunctionCall == nil {
		p.FunctionCall = p.FunctionCallSnake
	}
	if p.FunctionResponse == nil {
		p.FunctionResponse = p.FunctionResponseSnake
	}
	if d := p.InlineData; d != nil && d.MIMEType == "" {
		d.MIMEType = d.MIMETypeSnake
	}
	if d := p.FileData; d != nil {
		if d.MIMEType == "" {
			d.MIMEType = d.MIMETypeSnake
		}
		if d.FileURI == "" {
			d.FileURI = d.FileURISnake
		}
	}
}

func (gc *generationConfig) fillFromSnakeCase() {
	if gc.TopP == nil {
		gc.TopP = gc.TopPSnake
	}
	if gc.TopK == nil {
		gc.TopK = gc.TopKSnake
	}
	if gc.MaxOutputTokens == nil {
		gc.MaxOutputTokens = gc.MaxOutputTokensSnake
	}
	if gc.StopSequences == nil {
		gc.StopSequences = gc.StopSequencesSnake
	}
	if gc.FrequencyPenalty == nil {
		gc.FrequencyPenalty = gc.FrequencyPenaltySnake
	}
	if gc.PresencePenalty == nil {
		gc.PresencePenalty = gc.PresencePenaltySnake
	}
	if gc.ResponseMIMEType == "" {
		gc.ResponseMIMEType = gc.ResponseMIMETypeSnake
	}
	if gc.ResponseSchema == nil {
		gc.ResponseSchema = gc.ResponseSchemaSnake
	}
	if gc.ResponseJSONSchema == nil {
		gc.ResponseJSONSchema = gc.ResponseJSONSchemaSnake
	}
	if gc.ThinkingConfig == nil {
		gc.ThinkingConfig = gc.ThinkingConfigSnake
	}
	if tc := gc.ThinkingConfig; tc != nil && tc.ThinkingBudget == 0 {
		tc.ThinkingBudget = tc.ThinkingBudgetSnake
	}
}

func (t *geminiTool) fillFromSnakeCase() {
	if t.FunctionDeclarations == nil {
		t.FunctionDeclarations = t.FunctionDeclarationsSnake
	}
	for i := range t.FunctionDeclarations {
		if fd := &t.FunctionDeclarations[i]; fd.ParametersJSONSchema == nil {
			fd.ParametersJSONSchema = fd.ParametersJSONSchemaSnake
		}
	}
}

func (tc *toolConfig) fillFromSnakeCase() {
	if tc.FunctionCallingConfig == nil {
		tc.FunctionCallingConfig = tc.FunctionCallingConfigSnake
	}
	if fc := tc.FunctionCallingConfig; fc != nil && fc.AllowedFunctionNames == nil {
		fc.AllowedFunctionNames = fc.AllowedFunctionNamesSnake
	}
}
