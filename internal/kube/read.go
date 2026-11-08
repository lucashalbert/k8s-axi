package kube

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// typedAPI reads one built-in resource through its typed client.
type typedAPI struct {
	list func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error)
	get  func(ctx context.Context, cs kubernetes.Interface, ns, name string) (runtime.Object, error)
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

// typedResources are the built-in resources read through typed clients. Any
// other resource, and any other version of these, uses the dynamic client.
var typedResources = map[schema.GroupVersionResource]typedAPI{
	gvr("", "v1", "pods"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Pods(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().Pods(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "services"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Services(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().Services(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "endpoints"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Endpoints(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().Endpoints(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "configmaps"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().ConfigMaps(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().ConfigMaps(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "secrets"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Secrets(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().Secrets(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "persistentvolumeclaims"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "events"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Events(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.CoreV1().Events(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "nodes"): {
		func(ctx context.Context, cs kubernetes.Interface, _ string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Nodes().List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, _, n string) (runtime.Object, error) {
			return cs.CoreV1().Nodes().Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "namespaces"): {
		func(ctx context.Context, cs kubernetes.Interface, _ string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Namespaces().List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, _, n string) (runtime.Object, error) {
			return cs.CoreV1().Namespaces().Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("", "v1", "persistentvolumes"): {
		func(ctx context.Context, cs kubernetes.Interface, _ string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().PersistentVolumes().List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, _, n string) (runtime.Object, error) {
			return cs.CoreV1().PersistentVolumes().Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("apps", "v1", "deployments"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().Deployments(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("apps", "v1", "statefulsets"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().StatefulSets(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.AppsV1().StatefulSets(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("apps", "v1", "daemonsets"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().DaemonSets(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.AppsV1().DaemonSets(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("apps", "v1", "replicasets"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().ReplicaSets(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.AppsV1().ReplicaSets(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("batch", "v1", "jobs"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.BatchV1().Jobs(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.BatchV1().Jobs(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("batch", "v1", "cronjobs"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.BatchV1().CronJobs(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.BatchV1().CronJobs(ns).Get(ctx, n, metav1.GetOptions{})
		}},
	gvr("discovery.k8s.io", "v1", "endpointslices"): {
		func(ctx context.Context, cs kubernetes.Interface, ns string, o metav1.ListOptions) (runtime.Object, error) {
			return cs.DiscoveryV1().EndpointSlices(ns).List(ctx, o)
		},
		func(ctx context.Context, cs kubernetes.Interface, ns, n string) (runtime.Object, error) {
			return cs.DiscoveryV1().EndpointSlices(ns).Get(ctx, n, metav1.GetOptions{})
		}},
}

// listResult is one page of objects. More is true when the server holds
// further items that were not returned because of the list limit.
type listResult struct {
	Items     []unstructured.Unstructured
	More      bool
	Remaining *int64 // how many more, when the server says
}

// List reads resource r. ns is ignored for cluster-scoped resources and
// means all namespaces when empty.
func (c *Clients) List(ctx context.Context, r Resource, ns string, o metav1.ListOptions) (listResult, error) {
	if !r.Namespaced {
		ns = ""
	}
	if t, ok := typedResources[r.GVR]; ok {
		obj, err := t.list(ctx, c.Typed, ns, o)
		if err != nil {
			return listResult{}, err
		}
		return fromTyped(r, obj)
	}
	l, err := c.dynamicFor(r, ns).List(ctx, o)
	if err != nil {
		return listResult{}, err
	}
	return listResult{Items: l.Items, More: l.GetContinue() != "", Remaining: l.GetRemainingItemCount()}, nil
}

// Get reads one object of resource r. ns is ignored for cluster-scoped
// resources.
func (c *Clients) Get(ctx context.Context, r Resource, ns, name string) (*unstructured.Unstructured, error) {
	if !r.Namespaced {
		ns = ""
	}
	if t, ok := typedResources[r.GVR]; ok {
		obj, err := t.get(ctx, c.Typed, ns, name)
		if err != nil {
			return nil, err
		}
		return toUnstructured(r, obj)
	}
	return c.dynamicFor(r, ns).Get(ctx, name, metav1.GetOptions{})
}

func (c *Clients) dynamicFor(r Resource, ns string) dynamic.ResourceInterface {
	ri := c.Dynamic.Resource(r.GVR)
	if r.Namespaced && ns != "" {
		return ri.Namespace(ns)
	}
	return ri
}

// fromTyped converts a typed list to unstructured items.
func fromTyped(r Resource, list runtime.Object) (listResult, error) {
	objs, err := meta.ExtractList(list)
	if err != nil {
		return listResult{}, fmt.Errorf("read %s list: %w", r, err)
	}
	acc, err := meta.ListAccessor(list)
	if err != nil {
		return listResult{}, fmt.Errorf("read %s list: %w", r, err)
	}
	res := listResult{Items: make([]unstructured.Unstructured, 0, len(objs)), More: acc.GetContinue() != "", Remaining: acc.GetRemainingItemCount()}
	for _, o := range objs {
		u, err := toUnstructured(r, o)
		if err != nil {
			return listResult{}, err
		}
		res.Items = append(res.Items, *u)
	}
	return res, nil
}

// toUnstructured converts a typed object and stamps the apiVersion and kind
// that typed clients strip.
func toUnstructured(r Resource, obj runtime.Object) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r, err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(r.GVR.GroupVersion().String())
	u.SetKind(r.Kind)
	return u, nil
}
