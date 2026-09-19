package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

func decodeJSON(raw, empty string) (any, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = empty
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	v, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return v, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			var kvs []ocs.KV
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("activity: json object key")
				}
				val, err := decodeJSONValue(dec)
				if err != nil {
					return nil, err
				}
				kvs = append(kvs, ocs.K(key, val))
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return ocs.Obj(kvs...), nil
		case '[':
			arr := make([]any, 0)
			for dec.More() {
				val, err := decodeJSONValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("activity: json delim")
		}
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	case string, bool, nil:
		return v, nil
	default:
		return v, nil
	}
}
