package api

import (
	"encoding/json"
	"strings"
)

// Minimal OpenAPI 3.1 types used by the spec generation handler.

type openAPIDoc struct {
	OpenAPI    string                 `json:"openapi"`
	Info       openAPIInfo            `json:"info"`
	Servers    []openAPIServer        `json:"servers"`
	Paths      map[string]openAPIPath `json:"paths"`
	Components openAPIComponents      `json:"components"`
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type openAPIServer struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

type openAPIPath map[string]*openAPIOperation // key: "get"|"post"|"put"|"delete"|"patch"

type openAPIOperation struct {
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	OperationID string              `json:"operationId,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Security    []map[string][]any  `json:"security,omitempty"`
	Parameters  []openAPIParameter  `json:"parameters,omitempty"`
	RequestBody *openAPIRequestBody `json:"requestBody,omitempty"`
	Responses   map[string]any      `json:"responses"`
	// Extensions are the x- keys OpenAPI lets an operation carry. They are
	// written beside the standard fields, not nested, which is what the
	// specification requires and what a reader looks for.
	Extensions map[string]any `json:"-"`
}

func (o openAPIOperation) MarshalJSON() ([]byte, error) {
	type plain openAPIOperation
	b, err := json.Marshal(plain(o))
	if err != nil || len(o.Extensions) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range o.Extensions {
		if !strings.HasPrefix(k, "x-") {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		m[k] = raw
	}
	return json.Marshal(m)
}

type openAPIParameter struct {
	Name        string         `json:"name"`
	In          string         `json:"in"` // path | query | header
	Required    bool           `json:"required"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

type openAPIRequestBody struct {
	Required bool                        `json:"required"`
	Content  map[string]openAPIMediaType `json:"content"`
}

type openAPIMediaType struct {
	Schema map[string]any `json:"schema,omitempty"`
}

type openAPIComponents struct {
	SecuritySchemes map[string]any `json:"securitySchemes,omitempty"`
}
