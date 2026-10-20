package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkKeys rejects keys that match no config field, with the line they are
// on. A typo must stop startup: "aproval:" silently meaning "no approvals"
// is the worst way for a config to fail.
func checkKeys(n *yaml.Node, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Kind == yaml.DocumentNode {
		return checkKeys(n.Content[0], t, path)
	}
	if n.Kind != yaml.MappingNode || reflect.PointerTo(t).Implements(reflect.TypeFor[yaml.Unmarshaler]()) {
		return nil // scalars, sequences of scalars and custom types are checked by decoding
	}
	switch t.Kind() {
	case reflect.Map:
		for kv := range slices.Chunk(n.Content, 2) {
			if err := checkKeys(kv[1], t.Elem(), path+"."+kv[0].Value); err != nil {
				return err
			}
		}
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for field := range t.Fields() {
			if name, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		for kv := range slices.Chunk(n.Content, 2) {
			k := kv[0]
			ft, ok := fields[k.Value]
			if !ok {
				return fmt.Errorf("line %d: unknown key %q in %s (known: %s)", k.Line, k.Value,
					strings.TrimPrefix(path, "."), strings.Join(slices.Sorted(maps.Keys(fields)), ", "))
			}
			if err := checkKeys(kv[1], ft, path+"."+k.Value); err != nil {
				return err
			}
		}
	}
	return nil
}
