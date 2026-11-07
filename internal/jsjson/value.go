// Package jsjson reproduces JavaScript's JSON.parse and JSON.stringify (V8, Node 22) closely
// enough that the Go agent writes byte-identical files to the ones the TypeScript agent wrote.
//
// Why it exists (spec B §2): ApplyConfig skips a push when the config on disk equals
// JSON.stringify(parsedRequest, null, 2) byte for byte, and the hot roster swap compares
// compact JSON.stringify output of two configs. Go's encoding/json loses object key order
// (map[string]any), escapes <, >, &, U+2028/9 and prints numbers differently, so a port built on
// it would never see "unchanged" again and would reload sing-box (dropping every session) on
// every push. The same holds for any other "compare to what JSON.stringify would write" check
// (rear config, group F).
//
// Value model — a parsed JSON value is one of:
//
//	nil       null
//	bool      true / false
//	float64   every number (IEEE-754 double, as in JS)
//	string    UTF-8; a lone UTF-16 surrogate from a \uD8xx escape is kept as its WTF-8 bytes
//	          and printed back as the same lowercase escape
//	[]any     array
//	*Object   object, enumerated in V8 order (see Object)
package jsjson

import (
	"sort"
	"strconv"
)

// Object is a JS plain object: string keys with V8 enumeration order — array-index keys
// ("0".."4294967294" in canonical form) first in ascending numeric order, then every other key
// in insertion order. Re-setting an existing key keeps its position; deleting and re-adding moves
// it to the end. That is exactly what JSON.parse, object spread and Object.keys do in V8.
type Object struct {
	order []string
	m     map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{m: map[string]any{}} }

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.m)
}

// Get returns the value of key k.
func (o *Object) Get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.m[k]
	return v, ok
}

// Has reports whether key k exists.
func (o *Object) Has(k string) bool {
	_, ok := o.Get(k)
	return ok
}

// Set assigns k; a new key goes last in insertion order.
func (o *Object) Set(k string, v any) {
	if o.m == nil {
		o.m = map[string]any{}
	}
	if _, ok := o.m[k]; !ok {
		o.order = append(o.order, k)
	}
	o.m[k] = v
}

// Delete removes k.
func (o *Object) Delete(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, kk := range o.order {
		if kk == k {
			o.order = append(o.order[:i:i], o.order[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in V8 enumeration order (Object.keys).
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	var idx []string
	var named []string
	for _, k := range o.order {
		if _, ok := arrayIndex(k); ok {
			idx = append(idx, k)
		} else {
			named = append(named, k)
		}
	}
	if len(idx) == 0 {
		return append([]string(nil), named...)
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := arrayIndex(idx[i])
		b, _ := arrayIndex(idx[j])
		return a < b
	})
	return append(idx, named...)
}

// Clone is a shallow copy ({...o}).
func (o *Object) Clone() *Object {
	c := NewObject()
	for _, k := range o.Keys() {
		c.Set(k, o.m[k])
	}
	return c
}

// arrayIndex reports whether k is a canonical array index (0 … 2^32-2).
func arrayIndex(k string) (uint64, bool) {
	if k == "" || len(k) > 10 {
		return 0, false
	}
	if len(k) > 1 && k[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(k, 10, 64)
	if err != nil || n > 4294967294 {
		return 0, false
	}
	return n, true
}

// Spread is the object spread {...v}: a shallow copy of an object; an array or a string becomes
// an object keyed by index; null, booleans and numbers give an empty object.
func Spread(v any) *Object {
	switch t := v.(type) {
	case *Object:
		return t.Clone()
	case []any:
		o := NewObject()
		for i, e := range t {
			o.Set(strconv.Itoa(i), e)
		}
		return o
	case string:
		o := NewObject()
		for i, u := range utf16Units(t) {
			o.Set(strconv.Itoa(i), unitsToString([]uint16{u}))
		}
		return o
	}
	return NewObject()
}

// Equal reports deep equality of two values with JS === on primitives and the same key order on
// objects (what comparing two JSON.stringify outputs decides).
func Equal(a, b any) bool { return Stringify(a, "") == Stringify(b, "") }
