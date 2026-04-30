package ocs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type Version int

const (
	V1 Version = 1
	V2 Version = 2
)

type Format int

const (
	FormatXML Format = iota
	FormatJSON
)

const (
	StatusOKv1          = 100
	StatusOKv2          = 200
	RespondUnauthorised = 997
	RespondServerError  = 996
	RespondNotFound     = 998
	RespondUnknownError = 999
)

type Meta struct {
	Status       string
	StatusCode   int
	Message      string
	TotalItems   string
	ItemsPerPage string
}

type KV struct {
	Key   string
	Value any
}

type OrderedMap []KV

func K(key string, value any) KV { return KV{Key: key, Value: value} }

func Obj(kvs ...KV) OrderedMap { return OrderedMap(kvs) }

func (m OrderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, kv := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := encodeNoHTML(kv.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := marshalJSONValue(kv.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalJSONValue(v any) ([]byte, error) {
	switch val := v.(type) {
	case OrderedMap:
		return val.MarshalJSON()
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := marshalJSONValue(e)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	}
	if isRawMap(v) {
		return nil, fmt.Errorf("ocs: raw map not allowed in payload; use ocs.OrderedMap")
	}
	return encodeNoHTML(v)
}

func encodeNoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func isRawMap(v any) bool {
	switch v.(type) {
	case map[string]any, map[string]string, map[string]int:
		return true
	}
	return false
}

func Map(version Version, ocsCode int) int {
	switch version {
	case V1:
		if ocsCode == RespondUnauthorised {
			return http.StatusUnauthorized
		}
		return http.StatusOK
	case V2:
		switch ocsCode {
		case RespondUnauthorised:
			return http.StatusUnauthorized
		case RespondNotFound:
			return http.StatusNotFound
		case RespondServerError, RespondUnknownError:
			return http.StatusInternalServerError
		}
		if ocsCode < 200 || ocsCode > 600 {
			return http.StatusBadRequest
		}
		return ocsCode
	}
	return http.StatusInternalServerError
}

func NegotiateFormat(query, accept string) Format {
	if query != "" {
		if strings.EqualFold(query, "json") {
			return FormatJSON
		}
		return FormatXML
	}
	if strings.Contains(strings.ToLower(accept), "application/json") {
		return FormatJSON
	}
	return FormatXML
}

func Render(version Version, format Format, meta Meta, data any) ([]byte, string, error) {
	if meta.StatusCode == 0 {
		if version == V1 {
			meta.StatusCode = StatusOKv1
		} else {
			meta.StatusCode = StatusOKv2
		}
	}
	if meta.Status == "" {
		if meta.StatusCode == StatusOKv1 || meta.StatusCode == StatusOKv2 {
			meta.Status = "ok"
		} else {
			meta.Status = "failure"
		}
	}
	if meta.Message == "" && (meta.StatusCode == StatusOKv1 || meta.StatusCode == StatusOKv2) {
		meta.Message = "OK"
	}

	switch format {
	case FormatJSON:
		body, err := renderJSON(meta, data)
		return body, "application/json; charset=utf-8", err
	default:
		body, err := renderXML(meta, data)
		return body, "application/xml; charset=utf-8", err
	}
}

func renderJSON(meta Meta, data any) ([]byte, error) {
	metaKVs := OrderedMap{
		K("status", meta.Status),
		K("statuscode", meta.StatusCode),
		K("message", meta.Message),
	}
	if meta.TotalItems != "" {
		metaKVs = append(metaKVs, K("totalitems", meta.TotalItems))
	}
	if meta.ItemsPerPage != "" {
		metaKVs = append(metaKVs, K("itemsperpage", meta.ItemsPerPage))
	}
	envelope := Obj(K("ocs", Obj(
		K("meta", metaKVs),
		K("data", data),
	)))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(envelope); err != nil {
		return nil, fmt.Errorf("ocs: encode json: %w", err)
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return escapeSlashes(out), nil
}

func escapeSlashes(in []byte) []byte {
	out := make([]byte, 0, len(in)+8)
	for _, c := range in {
		if c == '/' {
			out = append(out, '\\', '/')
			continue
		}
		out = append(out, c)
	}
	return out
}

func renderXML(meta Meta, data any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0"?>` + "\n")
	buf.WriteString("<ocs>\n")
	buf.WriteString(" <meta>\n")
	buf.WriteString("  <status>" + xmlEscape(meta.Status) + "</status>\n")
	buf.WriteString("  <statuscode>" + strconv.Itoa(meta.StatusCode) + "</statuscode>\n")
	buf.WriteString("  <message>" + xmlEscape(meta.Message) + "</message>\n")
	if meta.TotalItems != "" {
		buf.WriteString("  <totalitems>" + xmlEscape(meta.TotalItems) + "</totalitems>\n")
	}
	if meta.ItemsPerPage != "" {
		buf.WriteString("  <itemsperpage>" + xmlEscape(meta.ItemsPerPage) + "</itemsperpage>\n")
	}
	buf.WriteString(" </meta>\n")
	if data == nil {
		buf.WriteString(" <data/>\n")
	} else {
		buf.WriteString(" <data>\n")
		if err := xmlEncodeValue(&buf, data, 2); err != nil {
			return nil, err
		}
		buf.WriteString(" </data>\n")
	}
	buf.WriteString("</ocs>\n")
	return buf.Bytes(), nil
}

func xmlEncodeValue(buf *bytes.Buffer, v any, depth int) error {
	indent := strings.Repeat(" ", depth)
	switch val := v.(type) {
	case OrderedMap:
		for _, kv := range val {
			if isComplex(kv.Value) {
				buf.WriteString(indent + "<" + kv.Key + ">\n")
				if err := xmlEncodeValue(buf, kv.Value, depth+1); err != nil {
					return err
				}
				buf.WriteString(indent + "</" + kv.Key + ">\n")
			} else {
				buf.WriteString(indent + "<" + kv.Key + ">" + xmlScalar(kv.Value) + "</" + kv.Key + ">\n")
			}
		}
	case []any:
		for _, e := range val {
			if isComplex(e) {
				buf.WriteString(indent + "<element>\n")
				if err := xmlEncodeValue(buf, e, depth+1); err != nil {
					return err
				}
				buf.WriteString(indent + "</element>\n")
			} else {
				buf.WriteString(indent + "<element>" + xmlScalar(e) + "</element>\n")
			}
		}
	default:
		if isRawMap(v) {
			return fmt.Errorf("ocs: raw map not allowed in payload; use ocs.OrderedMap")
		}
		buf.WriteString(indent + xmlScalar(v) + "\n")
	}
	return nil
}

func isComplex(v any) bool {
	switch v.(type) {
	case OrderedMap, []any:
		return true
	}
	return false
}

func xmlScalar(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return xmlEscape(val)
	case bool:
		if val {
			return "1"
		}
		return ""
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	default:
		return xmlEscape(fmt.Sprint(v))
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
