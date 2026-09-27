package httpapi

import (
	"bytes"
	"encoding"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
)

// marshalHTTP preserves encoding/json field selection and custom MarshalJSON
// contracts, while rendering actual time.Time values in the public UTC-ms form.
// It never guesses whether a free-form metadata string represents a timestamp.
func marshalHTTP(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var encoded any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&encoded); err != nil {
		return nil, err
	}
	return json.Marshal(httpTimes(reflect.ValueOf(value), encoded))
}

var timeType = reflect.TypeFor[time.Time]()
var marshalerType = reflect.TypeFor[json.Marshaler]()

func httpTimes(v reflect.Value, encoded any) any {
	if !v.IsValid() {
		return encoded
	}
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return encoded
		}
		v = v.Elem()
	}
	if v.Type() == timeType {
		return clock.Format(v.Interface().(time.Time))
	}
	if v.Kind() == reflect.Pointer && v.Type().Elem() == timeType {
		if v.IsNil() {
			return encoded
		}
		return clock.Format(v.Elem().Interface().(time.Time))
	}
	// A custom encoder owns its entire shape (for example commands.View).
	if v.Type().Implements(marshalerType) || v.CanAddr() && v.Addr().Type().Implements(marshalerType) {
		return encoded
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return encoded
		}
		return httpTimes(v.Elem(), encoded)
	}
	switch v.Kind() {
	case reflect.Struct:
		m, ok := encoded.(map[string]any)
		if !ok {
			return encoded
		}
		fields := map[string]jsonField{}
		collectJSONFields(v, 0, fields)
		for name, f := range fields {
			if f.ambiguous {
				continue
			}
			if prior, exists := m[name]; exists {
				m[name] = httpTimes(f.value, prior)
			}
		}
	case reflect.Map:
		m, ok := encoded.(map[string]any)
		if !ok {
			return encoded
		}
		iter := v.MapRange()
		for iter.Next() {
			name, ok := jsonMapKey(iter.Key())
			if !ok {
				continue
			}
			if prior, exists := m[name]; exists {
				m[name] = httpTimes(iter.Value(), prior)
			}
		}
	case reflect.Slice, reflect.Array:
		a, ok := encoded.([]any)
		if !ok {
			return encoded
		}
		for i := 0; i < v.Len() && i < len(a); i++ {
			a[i] = httpTimes(v.Index(i), a[i])
		}
	}
	return encoded
}

type jsonField struct {
	value             reflect.Value
	depth             int
	tagged, ambiguous bool
}

// Embedded fields obey the same nearest-depth / explicit-tag precedence as
// encoding/json. This matters for UploadView.Files shadowing Upload.Files.
func collectJSONFields(v reflect.Value, depth int, out map[string]jsonField) {
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		value := v.Field(i)
		if !value.CanInterface() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		tagged := name != ""
		if f.Anonymous && !tagged {
			inner := value
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					continue
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				collectJSONFields(inner, depth+1, out)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		next := jsonField{value: value, depth: depth, tagged: tagged}
		prior, exists := out[name]
		if !exists || depth < prior.depth || depth == prior.depth && tagged && !prior.tagged {
			out[name] = next
			continue
		}
		if depth == prior.depth && tagged == prior.tagged {
			prior.ambiguous = true
			out[name] = prior
		}
	}
}

func jsonMapKey(v reflect.Value) (string, bool) {
	if v.Kind() == reflect.String {
		return v.String(), true
	}
	if v.CanInterface() {
		if m, ok := v.Interface().(encoding.TextMarshaler); ok {
			b, err := m.MarshalText()
			return string(b), err == nil
		}
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(v.Uint(), 10), true
	}
	return "", false
}
