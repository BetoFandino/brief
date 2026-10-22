package projection

// Accessors for reading projected fields.
//
// Analyzers read objects through these rather than asserting their way down a
// map[string]any by hand. A missing path is an ordinary absence, never a panic:
// an analyzer runs against whatever the cluster actually sent, including
// objects from a CRD whose shape nobody promised.

// Value returns the value at a dotted path.
func (o Object) Value(path string) (any, bool) {
	if o.Fields == nil {
		return nil, false
	}
	return valueAt(o.Fields, path)
}

// Str returns a string field, or "" when absent or of another type.
func (o Object) Str(path string) string {
	v, ok := o.Value(path)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// Int returns a numeric field. Decoded JSON gives float64, and the typed
// clients give the integer kinds, so all of them are accepted.
func (o Object) Int(path string) (int64, bool) {
	v, ok := o.Value(path)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// IntOr returns a numeric field, or the fallback when absent. Useful where
// Kubernetes omits a zero: an absent readyReplicas means none are ready.
func (o Object) IntOr(path string, fallback int64) int64 {
	if n, ok := o.Int(path); ok {
		return n
	}
	return fallback
}

// Bool returns a boolean field.
func (o Object) Bool(path string) (bool, bool) {
	v, ok := o.Value(path)
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	return b, isBool
}

// List returns a slice field as maps, skipping entries of any other shape.
func (o Object) List(path string) []map[string]any {
	v, ok := o.Value(path)
	if !ok {
		return nil
	}
	raw, isSlice := v.([]any)
	if !isSlice {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, isMap := item.(map[string]any); isMap {
			out = append(out, m)
		}
	}
	return out
}

// Labels returns the object's labels.
func (o Object) Labels() map[string]string {
	v, ok := o.Value("metadata.labels")
	if !ok {
		return nil
	}
	raw, isMap := v.(map[string]any)
	if !isMap {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, isString := val.(string); isString {
			out[k] = s
		}
	}
	return out
}

// Condition finds a status condition by type, as Kubernetes shapes them.
func (o Object) Condition(condType string) (map[string]any, bool) {
	for _, c := range o.List("status.conditions") {
		if t, _ := c["type"].(string); t == condType {
			return c, true
		}
	}
	return nil, false
}

// Field reads a string out of one of the maps the List and Condition helpers
// return, so analyzers do not assert inline.
func Field(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Sub returns a nested map out of one of those maps.
func Sub(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		if cur == nil {
			return nil
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// Num reads a number out of one of those maps.
func Num(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	switch n := m[key].(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
