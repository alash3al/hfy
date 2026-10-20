package config

import (
	"fmt"
	"reflect"
	"sort"
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
		for i := 0; i < len(n.Content); i += 2 {
			if err := checkKeys(n.Content[i+1], t.Elem(), path+"."+n.Content[i].Value); err != nil {
				return err
			}
		}
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" && name != "-" {
				fields[name] = t.Field(i).Type
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			ft, ok := fields[k.Value]
			if !ok {
				known := make([]string, 0, len(fields))
				for name := range fields {
					known = append(known, name)
				}
				sort.Strings(known)
				return fmt.Errorf("line %d: unknown key %q in %s (known: %s)",
					k.Line, k.Value, strings.TrimPrefix(path, "."), strings.Join(known, ", "))
			}
			if err := checkKeys(n.Content[i+1], ft, path+"."+k.Value); err != nil {
				return err
			}
		}
	}
	return nil
}
