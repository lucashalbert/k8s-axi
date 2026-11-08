package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// maxManifestBytes caps all manifest input together, so stdin is bounded.
var maxManifestBytes = 16 << 20

// manifestObject is one object read from a manifest.
type manifestObject struct {
	obj    *unstructured.Unstructured
	source string // "deploy.yaml (document 2)", for errors
}

func manifestUsage(msg, hint string) error { return axierr.Usage("apply", msg, hint) }

// readManifests reads every object from the -f arguments: a file, a directory
// (yaml, yml and json files, not recursive, sorted by name) or "-" for stdin.
// Malformed input is a usage error and nothing is sent to the cluster.
func readManifests(paths []string, stdin io.Reader) ([]manifestObject, error) {
	if len(paths) == 0 {
		return nil, manifestUsage("no manifest given", "pass -f FILE, -f DIR or -f - for stdin")
	}
	budget := maxManifestBytes
	var out []manifestObject
	for _, p := range paths {
		files, err := manifestFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			data, name, err := readManifestFile(f, stdin, &budget)
			if err != nil {
				return nil, err
			}
			objs, err := decodeManifest(name, data)
			if err != nil {
				return nil, err
			}
			out = append(out, objs...)
		}
	}
	if len(out) == 0 {
		return nil, manifestUsage("the manifests contain no objects", "check the -f argument; empty and comment-only documents are skipped")
	}
	return out, nil
}

// manifestFiles expands one -f argument to the files it names.
func manifestFiles(p string) ([]string, error) {
	if p == "-" {
		return []string{p}, nil
	}
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, manifestUsage("manifest not found: "+p, "check the -f argument")
	case err != nil:
		return nil, axierr.Runtime("apply", p, err, "check that the path is readable")
	case !st.IsDir():
		return []string{p}, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, axierr.Runtime("apply", p, err, "check that the directory is readable")
	}
	var files []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			if !e.IsDir() {
				files = append(files, filepath.Join(p, e.Name()))
			}
		}
	}
	if len(files) == 0 {
		return nil, manifestUsage("no manifests (.yaml, .yml, .json) in "+p, "directories are not read recursively; name a file or a directory with manifests")
	}
	sort.Strings(files)
	return files, nil
}

func readManifestFile(f string, stdin io.Reader, budget *int) ([]byte, string, error) {
	name := f
	r := stdin
	if f == "-" {
		name = "stdin"
	} else {
		fh, err := os.Open(f)
		if err != nil {
			return nil, "", axierr.Runtime("apply", f, err, "check that the file is readable")
		}
		defer fh.Close()
		r = fh
	}
	if r == nil {
		return nil, "", manifestUsage("stdin is not available", "pass -f FILE instead")
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(*budget)+1))
	if err != nil {
		return nil, "", axierr.Runtime("apply", name, err, "check that the input is readable")
	}
	if len(data) > *budget {
		return nil, "", manifestUsage(fmt.Sprintf("the manifests are larger than %d MiB", maxManifestBytes>>20), "apply fewer objects at a time")
	}
	*budget -= len(data)
	return data, name, nil
}

// decodeManifest splits data into objects: YAML or JSON documents, with
// "List" documents flattened. Errors name the document, never its content.
func decodeManifest(name string, data []byte) ([]manifestObject, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []manifestObject
	for doc := 1; ; doc++ {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, manifestUsage(fmt.Sprintf("%s (document %d): %v", name, doc, err), "fix the YAML or JSON syntax")
		}
		if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
			continue
		}
		src := fmt.Sprintf("%s (document %d)", name, doc)
		var m map[string]any
		if err := utiljson.Unmarshal(raw, &m); err != nil {
			return nil, manifestUsage(src+" is not an object", "each document must be a Kubernetes object")
		}
		items, err := flattenList(src, m)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			o := &unstructured.Unstructured{Object: item}
			if err := checkShape(src, o); err != nil {
				return nil, err
			}
			out = append(out, manifestObject{obj: o, source: src})
		}
	}
}

// flattenList returns the items of a List document, or m itself.
func flattenList(src string, m map[string]any) ([]map[string]any, error) {
	kind, _ := m["kind"].(string)
	items, ok := m["items"].([]any)
	if !ok || !strings.HasSuffix(kind, "List") {
		return []map[string]any{m}, nil
	}
	out := make([]map[string]any, 0, len(items))
	for i, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			return nil, manifestUsage(fmt.Sprintf("%s: item %d is not an object", src, i+1), "each List item must be a Kubernetes object")
		}
		out = append(out, im)
	}
	return out, nil
}

// checkShape rejects objects server-side apply cannot address.
func checkShape(src string, o *unstructured.Unstructured) error {
	switch {
	case o.GetKind() == "":
		return manifestUsage(src+" has no kind", "add kind, for example 'kind: ConfigMap'")
	case o.GetAPIVersion() == "":
		return manifestUsage(src+" ("+o.GetKind()+") has no apiVersion", "add apiVersion, for example 'apiVersion: v1'")
	case o.GetName() == "" && o.GetGenerateName() != "":
		return manifestUsage(src+" ("+o.GetKind()+") sets generateName but no metadata.name",
			"server-side apply needs a name; set metadata.name, generateName is not supported")
	case o.GetName() == "":
		return manifestUsage(src+" ("+o.GetKind()+") has no metadata.name", "add metadata.name")
	}
	return nil
}
