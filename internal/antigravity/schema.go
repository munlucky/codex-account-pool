package antigravity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const maxSchemaBytes = 256 << 10

type schemaTransform struct {
	root  any
	refs  map[string]bool
	nodes int
	bytes int
}

// transformGoogleSchema handles schema positions only, never arbitrary data keys.
// Unsupported constraints fail locally, except uniqueItems (documented loss).
func transformGoogleSchema(raw any) (map[string]any, error) {
	t := &schemaTransform{root: raw, refs: map[string]bool{}}
	out, err := t.schema(raw, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxSchemaBytes {
		return nil, fmt.Errorf("schema expansion limit exceeded")
	}
	return out, nil
}

func (t *schemaTransform) schema(raw any, depth int) (map[string]any, error) {
	t.nodes++
	if depth > 32 || t.nodes > 4096 || t.bytes > maxSchemaBytes {
		return nil, fmt.Errorf("schema expansion limit exceeded")
	}
	s, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be an object")
	}
	out := map[string]any{}
	if value, exists := s["$ref"]; exists {
		ref, ok := value.(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("only local JSON Pointer references are supported")
		}
		if t.refs[ref] {
			return nil, fmt.Errorf("cyclic schema reference")
		}
		var target any = t.root
		for _, part := range strings.Split(ref[2:], "/") {
			for i := 0; i < len(part); i++ {
				if part[i] == '~' {
					if i+1 >= len(part) || (part[i+1] != '0' && part[i+1] != '1') {
						return nil, fmt.Errorf("invalid JSON Pointer escape")
					}
					i++
				}
			}
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			object, ok := target.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("unresolved schema reference")
			}
			target, ok = object[part]
			if !ok {
				return nil, fmt.Errorf("unresolved schema reference")
			}
		}
		t.refs[ref] = true
		var err error
		out, err = t.schema(target, depth+1)
		delete(t.refs, ref)
		if err != nil {
			return nil, err
		}
		for k, v := range s {
			switch k {
			case "$ref", "$defs", "definitions", "$schema", "$id", "default", "examples":
			case "description", "title":
				if _, ok := v.(string); !ok {
					return nil, fmt.Errorf("invalid schema annotation")
				}
				t.bytes += len(v.(string))
				out[k] = v
			default:
				if !reflect.DeepEqual(out[k], v) {
					return nil, fmt.Errorf("unsupported reference sibling constraint")
				}
			}
		}
		return out, nil
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := s[k]
		switch k {
		case "$schema", "$id", "$defs", "definitions", "default", "examples", "uniqueItems", "const":
			continue
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("properties must be an object")
			}
			clean := map[string]any{}
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				t.bytes += len(name)
				child, err := t.schema(props[name], depth+1)
				if err != nil {
					return nil, err
				}
				clean[name] = child
			}
			out[k] = clean
		case "items":
			child, err := t.schema(v, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = child
		case "anyOf":
			items, ok := v.([]any)
			if !ok || len(items) == 0 {
				return nil, fmt.Errorf("anyOf must be a nonempty array")
			}
			clean := make([]any, 0, len(items))
			for _, item := range items {
				child, err := t.schema(item, depth+1)
				if err != nil {
					return nil, err
				}
				clean = append(clean, child)
			}
			out[k] = clean
		case "additionalProperties":
			if flag, ok := v.(bool); ok {
				out[k] = flag
			} else {
				child, err := t.schema(v, depth+1)
				if err != nil {
					return nil, err
				}
				out[k] = child
			}
		case "type":
			typ, ok := v.(string)
			if !ok {
				list, yes := v.([]any)
				if !yes || len(list) != 2 {
					return nil, fmt.Errorf("unsupported type union")
				}
				for _, item := range list {
					if item == "null" {
						out["nullable"] = true
					} else {
						typ, ok = item.(string)
					}
				}
				if out["nullable"] != true || !ok {
					return nil, fmt.Errorf("unsupported type union")
				}
				if nullable, exists := s["nullable"]; exists && nullable == false {
					return nil, fmt.Errorf("nullable conflicts with type union")
				}
			}
			switch strings.ToLower(typ) {
			case "object", "array", "string", "number", "integer", "boolean", "null":
				out[k] = strings.ToLower(typ)
			default:
				return nil, fmt.Errorf("unsupported schema type")
			}
		case "enum", "required", "propertyOrdering":
			list, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("schema list must be an array")
			}
			if k == "enum" && len(list) == 0 {
				return nil, fmt.Errorf("enum must not be empty")
			}
			clean := make([]any, len(list))
			for i, item := range list {
				str, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("only string schema list values are supported")
				}
				t.bytes += len(str)
				clean[i] = str
			}
			out[k] = clean
		case "title", "description", "format", "pattern":
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("schema annotation must be a string")
			}
			t.bytes += len(str)
			out[k] = str
		case "nullable":
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("nullable must be boolean")
			}
			out[k] = v
		case "minimum", "maximum", "minItems", "maxItems", "minLength", "maxLength", "minProperties", "maxProperties":
			if _, ok := numericFloat(v); !ok {
				return nil, fmt.Errorf("schema bound must be numeric")
			}
			out[k] = v
		default:
			return nil, fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	if v, ok := s["const"]; ok {
		value, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("only string const is supported")
		}
		if typ, exists := out["type"]; exists && typ != "string" {
			return nil, fmt.Errorf("const conflicts with type")
		}
		if values, exists := out["enum"]; exists {
			found := false
			for _, item := range values.([]any) {
				if item == value {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("const conflicts with enum")
			}
		}
		out["type"] = "string"
		out["enum"] = []any{value}
		t.bytes += len(value)
	}
	if t.bytes > maxSchemaBytes {
		return nil, fmt.Errorf("schema expansion limit exceeded")
	}
	return out, nil
}
