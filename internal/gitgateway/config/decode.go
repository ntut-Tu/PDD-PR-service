package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

var durationType = reflect.TypeOf(time.Duration(0))

// decoder fills a struct from a merged YAML mapping. Unlike yaml.v3's own strict mode it reports
// every problem with the full YAML path of the key, and it reads durations as Go duration strings.
type decoder struct{ errs []error }

func newDecoder() *decoder { return &decoder{} }

func (d *decoder) decode(dst any, src map[string]any) error {
	d.mapping("", reflect.ValueOf(dst).Elem(), src)
	return errors.Join(d.errs...)
}

func (d *decoder) fail(path, format string, args ...any) {
	d.errs = append(d.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (d *decoder) mapping(path string, dst reflect.Value, src map[string]any) {
	fields := map[string]reflect.Value{}
	for i := 0; i < dst.NumField(); i++ {
		fields[dst.Type().Field(i).Tag.Get("yaml")] = dst.Field(i)
	}
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := join(path, key)
		field, known := fields[key]
		if !known {
			d.fail(child, "unknown key")
			continue
		}
		d.value(child, field, src[key])
	}
}

func (d *decoder) value(path string, dst reflect.Value, src any) {
	if dst.Kind() == reflect.Struct {
		m, ok := src.(map[string]any)
		if !ok {
			d.fail(path, "must be a mapping")
			return
		}
		d.mapping(path, dst, m)
		return
	}
	if _, isMap := src.(map[string]any); isMap {
		d.fail(path, "must be a single value, not a mapping")
		return
	}
	d.scalar(path, dst, src)
}

func (d *decoder) scalar(path string, dst reflect.Value, src any) {
	switch {
	case dst.Type() == durationType:
		s, ok := src.(string)
		if !ok {
			d.fail(path, "must be a duration string such as 30s")
			return
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			d.fail(path, "invalid duration %q, use Go syntax such as 500ms, 30s or 10m", s)
			return
		}
		dst.SetInt(int64(parsed))
	case dst.Kind() == reflect.String:
		s, ok := src.(string)
		if !ok {
			d.fail(path, "must be a string (quote it)")
			return
		}
		dst.SetString(s)
	case dst.Kind() == reflect.Bool:
		b, ok := src.(bool)
		if !ok {
			d.fail(path, "must be true or false")
			return
		}
		dst.SetBool(b)
	case dst.CanInt():
		n, ok := src.(int)
		if !ok {
			d.fail(path, "must be an integer")
			return
		}
		dst.SetInt(int64(n))
	case dst.CanFloat():
		switch n := src.(type) {
		case float64:
			dst.SetFloat(n)
		case int:
			dst.SetFloat(float64(n))
		default:
			d.fail(path, "must be a number")
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return strings.Join([]string{path, key}, ".")
}
