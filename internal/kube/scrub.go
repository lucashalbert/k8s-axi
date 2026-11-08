package kube

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/lucashalbert/axi-sdk-go/redact"
)

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// credName matches names that carry credentials, for env entries and
// ConfigMap keys.
var credName = regexp.MustCompile(`(?i)(passw(or)?d|secret|token|api[_-]?key|private[_-]?key|credential|access[_-]?key)`)

// scrubObject returns a copy of obj that is safe to print in full. There is
// deliberately no way to turn it off.
//
// Secret values become their sizes, managedFields and the last-applied
// annotation (which repeats Secret values verbatim) are dropped, literal
// values of credential-looking env entries and ConfigMap keys are masked, and
// every remaining string passes through redact.Mask.
func scrubObject(obj map[string]any) map[string]any {
	out := runtime.DeepCopyJSON(obj)
	u := &unstructured.Unstructured{Object: out}
	unstructured.RemoveNestedField(out, "metadata", "managedFields")
	if ann, ok, _ := unstructured.NestedMap(out, "metadata", "annotations"); ok {
		delete(ann, lastApplied)
		if len(ann) == 0 {
			unstructured.RemoveNestedField(out, "metadata", "annotations")
		} else {
			_ = unstructured.SetNestedMap(out, ann, "metadata", "annotations")
		}
	}
	switch {
	case isKind(u, "", "Secret"):
		for _, f := range []string{"data", "stringData"} {
			sizeOnly(out, f)
		}
	case isKind(u, "", "ConfigMap"):
		maskCredentialKeys(out, "data")
		maskCredentialKeys(out, "binaryData")
	}
	walk(out)
	return out
}

func isKind(u *unstructured.Unstructured, group, kind string) bool {
	gv := u.GroupVersionKind()
	return gv.Kind == kind && gv.Group == group
}

// sizeOnly replaces every value of the map at field with "<N bytes>".
func sizeOnly(obj map[string]any, field string) {
	m, ok, _ := unstructured.NestedMap(obj, field)
	if !ok {
		return
	}
	for k, v := range m {
		s, _ := v.(string)
		m[k] = fmt.Sprintf("<%d bytes>", valueSize(field, s))
	}
	_ = unstructured.SetNestedMap(obj, m, field)
}

// valueSize is the byte length of the value: data is base64, stringData is not.
func valueSize(field, s string) int {
	if field == "data" {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return len(b)
		}
	}
	return len(s)
}

func maskCredentialKeys(obj map[string]any, field string) {
	m, ok, _ := unstructured.NestedMap(obj, field)
	if !ok {
		return
	}
	for k := range m {
		if credName.MatchString(k) {
			m[k] = redact.Placeholder
		}
	}
	_ = unstructured.SetNestedMap(obj, m, field)
}

// walk masks credential env literals and secret-shaped strings in place.
func walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if n, ok := t["name"].(string); ok && credName.MatchString(n) {
			if _, ok := t["value"].(string); ok {
				t["value"] = redact.Placeholder
			}
		}
		for k, e := range t {
			t[k] = walk(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = walk(e)
		}
		return t
	case string:
		return redact.Mask(t)
	}
	return v
}

// keySize is a key of a Secret or ConfigMap with the size of its value.
type keySize struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// dataKeys lists the keys of obj's data, stringData and binaryData with
// value sizes, sorted by name. It never reads the values beyond their length.
func dataKeys(obj map[string]any) []keySize {
	var out []keySize
	for _, f := range []string{"data", "stringData", "binaryData"} {
		m, _, _ := unstructured.NestedMap(obj, f)
		for k, v := range m {
			s, _ := v.(string)
			size := len(s)
			if f != "stringData" && isSecretOrBinary(obj, f) {
				size = valueSize("data", s)
			}
			out = append(out, keySize{k, size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isSecretOrBinary reports whether field holds base64: Secret data and
// ConfigMap binaryData do, ConfigMap data does not.
func isSecretOrBinary(obj map[string]any, field string) bool {
	if field == "binaryData" {
		return true
	}
	kind, _ := obj["kind"].(string)
	return kind == "Secret"
}
