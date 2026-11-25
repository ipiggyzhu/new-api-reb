package privacy

// The same keys occur in the wire protocol, JSON Schema and arbitrary tool
// data. Track their position so a property named "file" or "name" does not
// inherit the meaning of a protocol field.
type fieldScope uint8

const (
	wireField fieldScope = iota
	toolDefinition
	toolDocumentation
	schemaNode
	schemaProperties
	schemaDefinitions
	structuralField
	toolData
	toolOutput
)

type jsonField struct {
	name     string
	scope    fieldScope
	property string // property whose schema may supply a literal default or example
	encoded  bool   // complete value of a JSON-encoded tool argument or output
}

func (s *Session) maskField(field jsonField, value string) string {
	if field.structural() {
		return value
	}
	if !field.encoded {
		return s.Mask(value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ref, plain := range s.Values {
		if value == plain {
			return ref
		}
	}
	return value
}

func (f jsonField) child(name, kind, role string, atRoot bool) jsonField {
	child := jsonField{name: name}
	switch f.scope {
	case toolData:
		child.scope = toolData
	case schemaProperties:
		return jsonField{name: name, scope: schemaNode, property: name}
	case schemaDefinitions:
		return jsonField{name: name, scope: schemaNode}
	case schemaNode:
		child.property = f.property
		switch name {
		case "properties", "patternProperties":
			child.scope = schemaProperties
		case "$defs", "definitions", "dependentSchemas":
			child.scope = schemaDefinitions
		case "items", "prefixItems", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "contains", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "propertyNames":
			child.scope = schemaNode
		case "type", "format", "required", "$ref", "$schema", "$id", "$anchor", "$dynamicRef", "$dynamicAnchor", "pattern", "dependentRequired":
			child.scope = structuralField
		case "description", "title", "$comment":
			child.scope = toolDocumentation
		case "default", "const", "enum", "examples", "example":
			child.scope, child.name = toolData, f.property
		default:
			child.scope = toolData
		}
	case toolDefinition:
		switch name {
		case "function", "tools", "functionDeclarations", "function_declarations":
			child.scope = toolDefinition
		case "parameters", "input_schema", "parametersJsonSchema", "parameters_json_schema", "response", "responseJsonSchema", "output_schema":
			child.scope = schemaNode
		case "name", "type":
			child.scope = structuralField
		case "description", "title":
			child.scope = toolDocumentation
		default:
			child.scope = toolData
		}
	case structuralField:
		child.scope = structuralField
	case toolOutput:
		if kind == "text" || kind == "input_text" || kind == "output_text" {
			if name == "text" {
				child.scope = toolData
			}
		} else {
			child.scope = toolData
		}
	default:
		switch {
		case atRoot && (name == "tools" || name == "functions"):
			child.scope = toolDefinition
		case name == "arguments" && (f.name == "function" || f.name == "function_call" || kind == "function_call"),
			name == "input" && (kind == "tool_use" || kind == "custom_tool_call"),
			name == "args" && (f.name == "functionCall" || f.name == "function_call"),
			name == "response" && (f.name == "functionResponse" || f.name == "function_response"):
			child.scope = toolData
		case name == "content" && (role == "tool" || role == "function" || kind == "tool_result"),
			name == "output" && (kind == "function_call_output" || kind == "custom_tool_call_output"):
			child.scope = toolOutput
		}
	}
	return child
}

func (f jsonField) structural() bool {
	return f.scope == structuralField || (f.scope == wireField && protocolField(f.name))
}
