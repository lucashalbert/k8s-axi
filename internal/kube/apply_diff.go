package kube

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// serverOwned are metadata fields the API server sets, which are not part of
// what an apply changes.
var serverOwned = []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp", "selfLink",
	"deletionTimestamp", "deletionGracePeriodSeconds"}

// changedPaths lists, sorted, the field paths whose values differ between
// live and proposed, ignoring server-owned metadata and status. Lists of the
// same length are compared per element, otherwise as one path. Only paths are
// returned, never values.
func changedPaths(live, proposed map[string]any) []string {
	var out []string
	diffValue("", comparableObject(live), comparableObject(proposed), &out)
	sort.Strings(out)
	return out
}

func comparableObject(obj map[string]any) map[string]any {
	c := runtime.DeepCopyJSON(obj)
	delete(c, "status")
	for _, f := range serverOwned {
		unstructured.RemoveNestedField(c, "metadata", f)
	}
	return c
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func diffValue(path string, a, b any, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, path)
			return
		}
		for k, x := range av {
			y, present := bv[k]
			if !present {
				*out = append(*out, joinPath(path, k))
				continue
			}
			diffValue(joinPath(path, k), x, y, out)
		}
		for k := range bv {
			if _, present := av[k]; !present {
				*out = append(*out, joinPath(path, k))
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*out = append(*out, path)
			return
		}
		for i := range av {
			diffValue(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, path)
		}
	}
}

// pathList joins at most max paths, saying how many were left out.
func pathList(paths []string, max int) string {
	if len(paths) <= max {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(paths[:max], ", "), len(paths)-max)
}

var managerOf = regexp.MustCompile(`conflict with "([^"]+)"`)

// conflictsOf lists the fields a server-side apply conflicted on and who owns
// them. It is nil for an error that is not a field-ownership conflict.
func conflictsOf(err error) []string {
	var se *apierrors.StatusError
	if !errors.As(err, &se) || !apierrors.IsConflict(se) || se.ErrStatus.Details == nil {
		return nil
	}
	var out []string
	for _, c := range se.ErrStatus.Details.Causes {
		if c.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		line := c.Field
		if m := managerOf.FindStringSubmatch(c.Message); m != nil {
			line += fmt.Sprintf(" (owned by %q)", m[1])
		}
		out = append(out, line)
	}
	return out
}

// secretKeys describes a Secret by its keys and the byte size of each value.
func secretKeys(o *unstructured.Unstructured) string {
	keys := dataKeys(o.Object)
	if len(keys) == 0 {
		return "no data keys"
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d B", k.Name, k.Bytes)
	}
	return "keys: " + strings.Join(parts, ", ")
}

// minScrub is the shortest Secret value removed from an error message; a
// shorter one would also mangle ordinary words.
const minScrub = 4

// scrubSecretValues removes the values of a Secret, as written and decoded,
// from msg. The commands never print a manifest or a live object, so this is
// the second layer, for text a server or webhook may echo back. Other kinds
// are returned unchanged.
func scrubSecretValues(msg string, o *unstructured.Unstructured) string {
	if o.GetKind() != "Secret" || o.GroupVersionKind().Group != "" {
		return msg
	}
	var vals []string
	for _, f := range []string{"data", "stringData"} {
		m, _, _ := unstructured.NestedMap(o.Object, f)
		for _, v := range m {
			s, _ := v.(string)
			vals = append(vals, s)
			if f == "data" {
				if b, err := base64.StdEncoding.DecodeString(s); err == nil {
					vals = append(vals, string(b))
				}
			}
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	var pairs []string
	for _, v := range vals {
		if len(v) >= minScrub {
			pairs = append(pairs, v, "<redacted>")
		}
	}
	if len(pairs) == 0 {
		return msg
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}
