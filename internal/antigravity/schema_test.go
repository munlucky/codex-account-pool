package antigravity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func schemaJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestGoogleSchemaResolvesLocalRefsWithoutMutatingInput(t *testing.T) {
	raw := schemaJSON(t, `{"$defs":{"a/b~c":{"type":"object","properties":{"$ref":{"const":"literal"},"uniqueItems":{"type":"boolean"}},"additionalProperties":false}},"$ref":"#/$defs/a~1b~0c"}`)
	before, _ := json.Marshal(raw)
	out, err := transformGoogleSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(raw)
	if string(before) != string(after) {
		t.Fatal("mutated caller schema")
	}
	props := out["properties"].(map[string]any)
	if props["$ref"].(map[string]any)["enum"].([]any)[0] != "literal" {
		t.Fatal(out)
	}
	if out["additionalProperties"] != false {
		t.Fatal(out)
	}
}
func TestGoogleSchemaSupportedForms(t *testing.T) {
	for _, source := range []string{
		`{"definitions":{"Text":{"type":"string"}},"type":"array","items":{"$ref":"#/definitions/Text"}}`,
		`{"type":["string","null"],"enum":["a","b"],"const":"b"}`,
		`{"anyOf":[{"type":"string"},{"type":"null"}]}`,
		`{"oneOf":[{"type":"string","enum":["all"]},{"type":"string","pattern":"^[1-9][0-9]*$"}]}`,
		`{"type":"object","additionalProperties":{"type":"string"}}`,
	} {
		out, err := transformGoogleSchema(schemaJSON(t, source))
		if err != nil {
			t.Fatal(source, err)
		}
		if strings.Contains(source, "oneOf") && out["anyOf"] == nil {
			t.Fatalf("expected oneOf to map to anyOf: %v", out)
		}
	}
}
func TestGoogleSchemaRejectsMeaningLossAndInvalidRefs(t *testing.T) {
	for _, source := range []string{
		`{"$ref":"#/$defs/missing"}`, `{"$ref":"https://example.com/schema"}`,
		`{"$defs":{"x":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x","type":"number"}`,
		`{"const":"x","enum":["y"]}`, `{"const":23}`, `{"enum":[{"$ref":"literal-data"}]}`,
		`{"allOf":[{"type":"string"}]}`, `{"type":["string","number"]}`, `false`,
		`{"$ref":"#/$defs/~2"}`, `{"not":{}}`,
		`{"enum":[]}`, `{"type":["string","null"],"nullable":false}`,
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := transformGoogleSchema(schemaJSON(t, source)); err == nil {
				t.Fatal("silently accepted", source)
			}
		})
	}
}
func TestGoogleSchemaExpansionIsBounded(t *testing.T) {
	defs := map[string]any{"d0": map[string]any{"type": "string"}}
	for i := 1; i < 16; i++ {
		ref := map[string]any{"$ref": fmt.Sprintf("#/$defs/d%d", i-1)}
		defs[fmt.Sprintf("d%d", i)] = map[string]any{"type": "object", "properties": map[string]any{"left": ref, "right": ref}}
	}
	if _, err := transformGoogleSchema(map[string]any{"$defs": defs, "$ref": "#/$defs/d15"}); err == nil {
		t.Fatal("expansion accepted")
	}
	if _, err := transformGoogleSchema(map[string]any{"$defs": map[string]any{"x": map[string]any{"type": "string"}}, "$ref": "#/$defs/x", "description": strings.Repeat("x", maxSchemaBytes+1)}); err == nil {
		t.Fatal("large sibling accepted")
	}
}
func TestGoogleSchemaConstAndAnnotations(t *testing.T) {
	out, err := transformGoogleSchema(schemaJSON(t, `{"const":"ok","default":"wrong","examples":["sample"],"$schema":"ignored","$id":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, map[string]any{"type": "string", "enum": []any{"ok"}}) {
		t.Fatal(out)
	}
}
